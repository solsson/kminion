package minion

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sync"
	"time"

	"github.com/cloudhut/kminion/v2/kafka"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/kversion"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

type Service struct {
	Cfg    Config
	logger *zap.Logger

	// requestGroup is used to deduplicate multiple concurrent requests to kafka
	requestGroup *singleflight.Group
	cache        map[string]interface{}
	cacheLock    sync.RWMutex

	AllowedGroupIDsExpr []*regexp.Regexp
	IgnoredGroupIDsExpr []*regexp.Regexp
	AllowedTopicsExpr   []*regexp.Regexp
	IgnoredTopicsExpr   []*regexp.Regexp

	kafkaSvc *kafka.Service
	storage  *Storage

	// EndToEnd
	minionID               string  // unique identifier, reported in metrics, in case multiple instances run at the same time
	lastRoundtripTimestamp float64 // creation time (in utc ms) of the message that most recently passed the roundtripSla check

	// EndToEnd Metrics
	endToEndMessagesProduced  prometheus.Counter
	endToEndMessagesAcked     prometheus.Counter
	endToEndMessagesReceived  prometheus.Counter
	endToEndMessagesCommitted prometheus.Counter

	endToEndAckLatency       prometheus.Histogram
	endToEndRoundtripLatency prometheus.Histogram
	endToEndCommitLatency    prometheus.Histogram
}

func NewService(cfg Config, logger *zap.Logger, kafkaSvc *kafka.Service, metricNamespace string) (*Service, error) {
	storage, err := newStorage(logger)

	if err != nil {
		return nil, fmt.Errorf("failed to create storage: %w", err)
	}

	// Compile regexes. We can ignore the errors because valid compilation has been validated already
	allowedGroupIDsExpr, _ := compileRegexes(cfg.ConsumerGroups.AllowedGroupIDs)
	ignoredGroupIDsExpr, _ := compileRegexes(cfg.ConsumerGroups.IgnoredGroupIDs)
	allowedTopicsExpr, _ := compileRegexes(cfg.Topics.AllowedTopics)
	ignoredTopicsExpr, _ := compileRegexes(cfg.Topics.IgnoredTopics)

	service := &Service{
		Cfg:    cfg,
		logger: logger,

		requestGroup: &singleflight.Group{},
		cache:        make(map[string]interface{}),
		cacheLock:    sync.RWMutex{},

		AllowedGroupIDsExpr: allowedGroupIDsExpr,
		IgnoredGroupIDsExpr: ignoredGroupIDsExpr,
		AllowedTopicsExpr:   allowedTopicsExpr,
		IgnoredTopicsExpr:   ignoredTopicsExpr,

		kafkaSvc: kafkaSvc,
		storage:  storage,

		minionID:               uuid.NewString(),
		lastRoundtripTimestamp: 0,
	}

	// End-to-End metrics
	if cfg.EndToEnd.Enabled {
		makeCounter := func(name string, help string) prometheus.Counter {
			return promauto.NewCounter(prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: "end_to_end",
				Name:      name,
				Help:      help,
			})
		}
		makeHistogram := func(name string, maxLatency time.Duration, help string) prometheus.Histogram {
			return promauto.NewHistogram(prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Subsystem: "end_to_end",
				Name:      name,
				Help:      help,
				Buckets:   createHistogramBuckets(maxLatency),
			})
		}

		// Low-level info
		// Users can construct alerts like "can't produce messages" themselves from those
		service.endToEndMessagesProduced = makeCounter("messages_produced_total", "Number of messages that kminion's end-to-end test has tried to send to kafka")
		service.endToEndMessagesAcked = makeCounter("messages_acked_total", "Number of messages kafka acknowledged as produced")
		service.endToEndMessagesReceived = makeCounter("messages_received_total", "Number of *matching* messages kminion received. Every roundtrip message has a minionID (randomly generated on startup) and a timestamp. Kminion only considers a message a match if it it arrives within the configured roundtrip SLA (and it matches the minionID)")
		service.endToEndMessagesCommitted = makeCounter("messages_committed_total", "Number of *matching* messages kminion successfully commited as read/processed. See 'messages_received' for what 'matching' means. Kminion will commit late/mismatching messages to kafka as well, but those won't be counted in this metric.")

		// Latency Histograms
		// More detailed info about how long stuff took
		// Since histograms also have an 'infinite' bucket, they can be used to detect small hickups "lost" messages
		service.endToEndAckLatency = makeHistogram("produce_latency_seconds", cfg.EndToEnd.Producer.AckSla, "Time until we received an ack for a produced message")
		service.endToEndRoundtripLatency = makeHistogram("roundtrip_latency_seconds", cfg.EndToEnd.Consumer.RoundtripSla, "Time it took between sending (producing) and receiving (consuming) a message")
		service.endToEndCommitLatency = makeHistogram("commit_latency_seconds", cfg.EndToEnd.Consumer.CommitSla, "Time kafka took to respond to kminion's offset commit")
	}

	return service, nil
}

func (s *Service) Start(ctx context.Context) error {
	err := s.ensureCompatibility(ctx)
	if err != nil {
		return fmt.Errorf("failed to check feature compatibility against Kafka: %w", err)
	}

	if s.Cfg.ConsumerGroups.ScrapeMode == ConsumerGroupScrapeModeOffsetsTopic {
		go s.startConsumingOffsets(ctx)
	}

	if s.Cfg.EndToEnd.Enabled {
		go s.initEndToEnd(ctx)
	}

	return nil
}

func (s *Service) ensureCompatibility(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	versionsRes, err := s.GetAPIVersions(ctx)
	if err != nil {
		return fmt.Errorf("kafka api versions couldn't be fetched: %w", err)
	}
	versions := kversion.FromApiVersionsResponse(versionsRes)

	// Check Describe Log Dirs
	if s.Cfg.LogDirs.Enabled {
		k := kmsg.NewDescribeLogDirsRequest()
		isSupported := versions.HasKey(k.Key())
		if !isSupported {
			s.logger.Warn("describing log dirs is enabled, but it is not supported because your Kafka cluster " +
				"version is too old. feature will be disabled")
			s.Cfg.LogDirs.Enabled = false
		}
	}

	return nil
}

func (s *Service) getCachedItem(key string) (interface{}, bool) {
	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	val, exists := s.cache[key]
	return val, exists
}

func (s *Service) setCachedItem(key string, val interface{}, timeout time.Duration) {
	s.cacheLock.Lock()
	defer s.cacheLock.Unlock()

	go func() {
		time.Sleep(timeout)
		s.deleteCachedItem(key)
	}()

	s.cache[key] = val
}

func (s *Service) deleteCachedItem(key string) {
	s.cacheLock.Lock()
	defer s.cacheLock.Unlock()

	delete(s.cache, key)
}

// create histogram buckets for metrics reported by 'end-to-end'
// todo:
/*
- custom, much simpler, exponential buckets
  we know:
  	- we want to go from 5ms to 'max'
	- we want to double each time
	- doubling 5ms might not get us to 'max' exactly
  questions:
	- can we slightly adjust the factor so we hit 'max' exactly?
	- or can we adjust 'max'?
		(and if so, better to overshoot or undershoot?)
	- or should we just set the last bucket to 'max' exactly?
*/
func createHistogramBuckets(maxLatency time.Duration) []float64 {
	// Since this is an exponential bucket we need to take Log base2 or binary as the upper bound
	// Divide by 10 for the argument because the base is counted as 20ms and we want to normalize it as base 2 instead of 20
	// +2 because it starts at 5ms or 0.005 sec, to account 5ms and 10ms before it goes to the base which in this case is 0.02 sec or 20ms
	// and another +1 to account for decimal points on int parsing
	latencyCount := math.Logb(float64(maxLatency.Milliseconds() / 10))
	count := int(latencyCount) + 3
	bucket := prometheus.ExponentialBuckets(0.005, 2, count)

	return bucket
}

// called from e2e when a message is acknowledged
func (s *Service) onAck(partitionId int32, duration time.Duration) {
	s.endToEndMessagesAcked.Inc()
	s.endToEndAckLatency.Observe(duration.Seconds())
}

// called from e2e when a message completes a roundtrip (send to kafka, receive msg from kafka again)
func (s *Service) onRoundtrip(partitionId int32, duration time.Duration) {
	if duration > s.Cfg.EndToEnd.Consumer.RoundtripSla {
		return // message is too old
	}

	// todo: track "lastRoundtripMessage"
	// if msg.Timestamp < s.lastRoundtripTimestamp {
	// 	return // msg older than what we recently processed (out of order, should never happen)
	// }

	s.endToEndMessagesReceived.Inc()
	s.endToEndRoundtripLatency.Observe(duration.Seconds())
}

// called from e2e when an offset commit is confirmed
func (s *Service) onOffsetCommit(partitionId int32, duration time.Duration) {

	// todo:
	// if the commit took too long, don't count it in 'commits' but add it to the histogram?
	// and how do we want to handle cases where we get an error??
	// should we have another metric that tells us about failed commits? or a label on the counter?

	s.endToEndCommitLatency.Observe(duration.Seconds())

	if duration > s.Cfg.EndToEnd.Consumer.CommitSla {
		return
	}

	s.endToEndMessagesCommitted.Inc()
}
