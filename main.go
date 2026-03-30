package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bsm/redislock"
	"github.com/gofiber/fiber/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sigmavirus24/circuitry"
)

// ============================================================
// Config
// ============================================================

const (
	// External API yang akan dipanggil (bisa diganti ke host yang salah untuk simulasi failure)
	externalAPIURL = "https://jsonplaceholder.typicode.com/posts/1"

	// Redis
	redisClusterToggle      = true
	redisSingleInstanceAddr = "localhost:6379"
	redisClusterAddr        = "localhost:6379"
	redisDB                 = 1

	// Circuit breaker
	circuitName        = "external-api"
	failureThreshold   = 3  // trip ke OPEN setelah >3 consecutive failures
	closeThreshold     = 2  // kembali ke CLOSED setelah 2 consecutive successes di HALF-OPEN
	allowAfterSeconds  = 15 // pindah dari OPEN ke HALF-OPEN setelah 15 detik
	cyclicClearSeconds = 300
	lockTTLSeconds     = 10
)

// ============================================================
// Custom Redis Backend (fix upstream bug: lock & data key collision)
// ============================================================

type redisBackend struct {
	client   redis.UniversalClient
	locker   *redislock.Client
	lockOpts *redislock.Options
	lockTTL  time.Duration
}

type redLock struct {
	ctx  context.Context
	lock *redislock.Lock
}

func (l *redLock) Lock()   {}
func (l *redLock) Unlock() { _ = l.lock.Release(l.ctx) }

// Hash tag {cb} memastikan lock & data key berada di slot yang sama (untuk Redis Cluster)
func dataKey(name string) string { return "{cb}:data:" + name }
func lockKey(name string) string { return "{cb}:lock:" + name }

func (b *redisBackend) Store(ctx context.Context, name string, ci circuitry.CircuitInformation) error {
	bytes, _ := json.Marshal(ci)
	return b.client.SetArgs(ctx, dataKey(name), string(bytes), redis.SetArgs{ExpireAt: ci.ExpiresAfter}).Err()
}

func (b *redisBackend) Retrieve(ctx context.Context, name string) (circuitry.CircuitInformation, error) {
	val, err := b.client.Get(ctx, dataKey(name)).Result()
	ci := circuitry.CircuitInformation{}
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return ci, nil
		}
		return ci, err
	}
	if err := json.Unmarshal([]byte(val), &ci); err != nil {
		return circuitry.CircuitInformation{}, err
	}
	return ci, nil
}

func (b *redisBackend) Lock(ctx context.Context, name string) (sync.Locker, error) {
	lock, err := b.locker.Obtain(ctx, lockKey(name), b.lockTTL, b.lockOpts)
	if err != nil {
		return nil, err
	}
	return &redLock{ctx, lock}, nil
}

// ============================================================
// Circuit Breaker Factory
// ============================================================

func newCBFactory(redisClient redis.UniversalClient) *circuitry.CircuitBreakerFactory {
	backend := &redisBackend{
		client: redisClient,
		locker: redislock.New(redisClient),
		lockOpts: &redislock.Options{
			RetryStrategy: redislock.LimitRetry(redislock.ExponentialBackoff(50*time.Millisecond, 500*time.Millisecond), 5),
		},
		lockTTL: time.Duration(lockTTLSeconds) * time.Second,
	}

	settings, err := circuitry.NewFactorySettings(
		circuitry.WithStorageBackend(backend),
		circuitry.WithDefaultNameFunc(),
		circuitry.WithDefaultTripFunc(),
		circuitry.WithDefaultFallbackErrorMatcher(),
		circuitry.WithFailureCountThreshold(failureThreshold),
		circuitry.WithCloseThreshold(closeThreshold),
		circuitry.WithAllowAfter(time.Duration(allowAfterSeconds)*time.Second),
		circuitry.WithCyclicClearAfter(time.Duration(cyclicClearSeconds)*time.Second),
		circuitry.WithStateChangeCallback(func(name string, _ map[string]any, from, to circuitry.CircuitState) {
			log.Printf("⚡ CB STATE CHANGE [%s]: %s → %s\n", name, from.String(), to.String())
		}),
	)
	if err != nil {
		log.Fatal("failed to create CB factory:", err)
	}

	return circuitry.NewCircuitBreakerFactory(settings)
}

// ============================================================
// External API Call (yang dilindungi CB)
// ============================================================

func fetchPost(ctx context.Context, url string) (interface{}, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var post interface{}
	if err := json.Unmarshal(body, &post); err != nil {
		return nil, fmt.Errorf("unmarshal failed: %w", err)
	}
	return &post, nil
}

// ============================================================
// Generic CB Execute helper
// ============================================================

func cbExecute[T any](ctx context.Context, breaker circuitry.CircuitBreaker, fn func() (T, error)) (T, error) {
	var zero T
	if err := breaker.Start(ctx); err != nil {
		return zero, err
	}
	result, workErr := fn()
	breaker.End(ctx, workErr)
	if workErr != nil {
		return zero, workErr
	}
	return result, nil
}

// ============================================================
// Main
// ============================================================

// apiURL bisa diubah runtime via /config endpoint untuk simulasi failure
var apiURL = externalAPIURL

// newRedisClient creates a redis.UniversalClient based on cluster toggle.
func newRedisClient() redis.UniversalClient {
	if redisClusterToggle {
		log.Println("circuit breaker using Redis Cluster")
		return redis.NewClusterClient(&redis.ClusterOptions{
			Addrs: []string{redisClusterAddr},
			// Password: redisPassword,
		})
	}

	log.Println("circuit breaker using Redis single-instance")
	return redis.NewClient(&redis.Options{
		Addr: redisSingleInstanceAddr,
		// Password: redisPassword,
		DB: redisDB,
	})
}

func main() {
	// Redis connection
	redisClient := newRedisClient()
	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Fatal("redis connection failed:", err)
	}
	log.Println("✅ Redis connected")

	// Circuit breaker factory
	cbFactory := newCBFactory(redisClient)
	log.Printf("✅ Circuit breaker ready (failure_threshold=%d, close_threshold=%d, allow_after=%ds)\n",
		failureThreshold, closeThreshold, allowAfterSeconds)

	app := fiber.New()

	// GET /post — panggil external API dengan CB protection
	app.Get("/post", func(c *fiber.Ctx) error {
		breaker := cbFactory.BreakerFor(circuitName, nil)

		post, err := cbExecute(c.Context(), breaker, func() (interface{}, error) {
			return fetchPost(c.Context(), apiURL)
		})
		if err != nil {
			log.Printf("❌ Request failed: %s\n", err)
			return c.Status(503).JSON(fiber.Map{
				"error": err.Error(),
			})
		}

		log.Println("✅ Request success")
		return c.JSON(post)
	})

	// GET /state — lihat state CB saat ini
	app.Get("/state", func(c *fiber.Ctx) error {
		val, err := redisClient.Get(c.Context(), dataKey(circuitName)).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return c.JSON(fiber.Map{"state": "no data yet (CLOSED)"})
			}
			return c.Status(500).JSON(fiber.Map{"error": err.Error()})
		}

		var info map[string]any
		json.Unmarshal([]byte(val), &info)

		stateNames := map[float64]string{0: "CLOSED", 1: "OPEN", 2: "HALF-OPEN"}
		if s, ok := info["state"].(float64); ok {
			info["state_name"] = stateNames[s]
		}

		return c.JSON(info)
	})

	// POST /config — ubah target URL untuk simulasi failure
	app.Post("/config", func(c *fiber.Ctx) error {
		var body struct {
			URL string `json:"url"`
		}
		if err := c.BodyParser(&body); err != nil || body.URL == "" {
			return c.Status(400).JSON(fiber.Map{"error": "provide {\"url\": \"...\"}"})
		}
		apiURL = body.URL
		log.Printf("🔧 API URL changed to: %s\n", apiURL)
		return c.JSON(fiber.Map{"url": apiURL})
	})

	// POST /reset — reset CB state
	app.Post("/reset", func(c *fiber.Ctx) error {
		redisClient.Del(c.Context(), dataKey(circuitName))
		redisClient.Del(c.Context(), lockKey(circuitName))
		apiURL = externalAPIURL
		log.Println("🔄 CB state reset, URL restored")
		return c.JSON(fiber.Map{"status": "reset ok", "url": apiURL})
	})

	log.Println("🚀 Demo server starting on :3000")
	log.Println("")
	log.Println("Endpoints:")
	log.Println("  GET  /post   — panggil external API (dilindungi CB)")
	log.Println("  GET  /state  — lihat state CB di Redis")
	log.Println("  POST /config — ubah target URL {\"url\": \"http://localhost:9999\"}")
	log.Println("  POST /reset  — reset CB state & kembalikan URL")
	log.Println("")
	log.Println("Skenario demo:")
	log.Println("  1. GET /post beberapa kali (sukses, CLOSED)")
	log.Println("  2. POST /config {\"url\": \"http://localhost:9999\"} (arahkan ke host mati)")
	log.Printf("  3. GET /post %d+ kali (gagal, trip ke OPEN)\n", failureThreshold+1)
	log.Println("  4. GET /post (fast-fail, CB OPEN)")
	log.Printf("  5. Tunggu %d detik (OPEN → HALF-OPEN)\n", allowAfterSeconds)
	log.Println("  6. POST /reset (kembalikan URL)")
	log.Printf("  7. GET /post %d kali (sukses, HALF-OPEN → CLOSED)\n", closeThreshold)

	log.Fatal(app.Listen(":3000"))
}
