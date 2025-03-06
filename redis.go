package mercure

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
	"golang.org/x/net/context"
)

const (
	lastEventIDKey = "lastEventID"
	publishScript  = `
		redis.call("SET", KEYS[1], ARGV[1])
		redis.call("PUBLISH", ARGV[2], ARGV[3])
		return true
	`
)

type RedisTransport struct {
	sync.RWMutex
	logger             Logger
	client             *redis.Client
	subscribers        *SubscriberList
	dispatcherPoolSize int
	dispatcher         chan SubscriberPayload
	closing            chan any
	closed             chan any
	publishScript      *redis.Script
	closedOnce         sync.Once
	redisChannel       string
	redisSubscriberCtx context.Context
}

type SubscriberPayload struct {
	subscriber *LocalSubscriber
	payload    Update
}

func NewRedisTransport(
	logger Logger,
	address string,
	username string,
	password string,
	subscribersSize int,
	dispatcherPoolSize int,
	redisChannel string,
) (*RedisTransport, error) {
	client := redis.NewClient(&redis.Options{
		Username: username,
		Password: password,
		Addr:     address,
	})

	if pong := client.Ping(context.Background()); pong.String() != "ping: PONG" {
		return nil, fmt.Errorf("failed to connect to Redis: %w", pong.Err())
	}

	return NewRedisTransportInstance(logger, client, subscribersSize, dispatcherPoolSize, redisChannel)
}

func NewRedisTransportInstance(
	logger Logger,
	client *redis.Client,
	subscribersSize int,
	dispatcherPoolSize int,
	redisChannel string,
) (*RedisTransport, error) {
	subscriber := client.PSubscribe(context.Background(), redisChannel)

	subscribeCtx, subscribeCancel := context.WithCancel(context.Background())

	transport := &RedisTransport{
		logger:             logger,
		client:             client,
		subscribers:        NewSubscriberList(subscribersSize),
		dispatcherPoolSize: dispatcherPoolSize,
		publishScript:      redis.NewScript(publishScript),
		redisChannel:       redisChannel,
		closed:             make(chan any),
		closing:            make(chan any),
		dispatcher:         make(chan SubscriberPayload),
		redisSubscriberCtx: subscribeCtx,
	}

	go func() {
		defer subscribeCancel()
		select {
		case <-transport.closing:
			if err := subscriber.Close(); err != nil && err != redis.ErrClosed {
				logger.Error(err.Error())
			}
		case <-transport.closed:
		case <-subscribeCtx.Done():
		}
	}()
	go transport.subscribe(subscribeCtx, subscriber)

	wg := sync.WaitGroup{}
	wg.Add(dispatcherPoolSize)
	for range dispatcherPoolSize {
		go transport.dispatch(&wg)
	}
	go func() {
		wg.Wait()
		close(transport.dispatcher)
	}()

	return transport, nil
}

func (u Update) MarshalBinary() ([]byte, error) {
	bytes, err := json.Marshal(u)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal: %w", err)
	}

	return bytes, nil
}

func (t *RedisTransport) Dispatch(update *Update) error {
	select {
	case <-t.closed:

		return ErrClosedTransport
	default:
	}

	AssignUUID(update)

	keys := []string{lastEventIDKey}
	arguments := []interface{}{update.ID, t.redisChannel, update}
	_, err := t.publishScript.Run(context.Background(), t.client, keys, arguments...).Result()
	if err != nil {
		return fmt.Errorf("redis failed to publish: %w", err)
	}

	return nil
}

func (t *RedisTransport) AddSubscriber(s *LocalSubscriber) error {
	select {
	case <-t.closed:
		return ErrClosedTransport
	default:
	}
	t.Lock()
	t.subscribers.Add(s)
	t.Unlock()

	s.Ready()

	return nil
}

func (t *RedisTransport) RemoveSubscriber(s *LocalSubscriber) error {
	select {
	case <-t.closed:
		return ErrClosedTransport
	default:
	}
	t.Lock()
	defer t.Unlock()
	t.subscribers.Remove(s)

	return nil
}

func (t *RedisTransport) GetSubscribers() (string, []*Subscriber, error) {
	select {
	case <-t.closed:
		return "", nil, ErrClosedTransport
	default:
	}
	t.RLock()
	defer t.RUnlock()
	lastEventID, err := t.client.Get(context.Background(), lastEventIDKey).Result()
	if err != nil {
		return "", nil, fmt.Errorf("redis failed to get last event id: %w", err)
	}

	return lastEventID, getSubscribers(t.subscribers), nil
}

func (t *RedisTransport) Close() (err error) {
	t.closedOnce.Do(func() {
		t.Lock()
		defer t.Unlock()
		t.subscribers.Walk(0, func(s *LocalSubscriber) bool {
			s.Disconnect()

			return true
		})
		close(t.closing)
		<-t.redisSubscriberCtx.Done()
		err = t.client.Close()
		if err != nil {
			t.logger.Error(fmt.Errorf("unable to close: %w", err).Error())
		}
		close(t.closed)
	})

	return nil
}

func (t *RedisTransport) subscribe(ctx context.Context, subscriber *redis.PubSub) {
	for {
		message, err := subscriber.ReceiveMessage(ctx)
		if err != nil {
			if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, redis.ErrClosed) {
				return
			}

			t.logger.Error(err.Error())

			continue
		}
		var update Update
		if err := json.Unmarshal([]byte(message.Payload), &update); err != nil {
			t.logger.Error(err.Error())

			continue
		}
		topics := []string{}
		topics = append(topics, update.Topics...)
		t.Lock()
		for _, subscriber := range t.subscribers.MatchAny(&update) {
			update.Topics = topics
			t.dispatcher <- SubscriberPayload{subscriber, update}
		}
		t.Unlock()
	}
}

func (t *RedisTransport) dispatch(wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case message := <-t.dispatcher:
			message.subscriber.Dispatch(&message.payload, false)
		case <-t.closed:

			return
		}
	}
}

var (
	_ Transport            = (*RedisTransport)(nil)
	_ TransportSubscribers = (*RedisTransport)(nil)
)
