# Circuit Breaker Demo

Demo standalone untuk menunjukkan cara kerja circuit breaker pattern menggunakan
`sigmavirus24/circuitry` dengan custom Redis backend.

Demo ini berdiri sendiri (punya `go.mod` terpisah) dan tidak bergantung pada
kode project utama `core-middleware.idn.media`.

---

## Daftar Isi

- [Prasyarat](#prasyarat)
- [Cara Menjalankan](#cara-menjalankan)
- [Konfigurasi](#konfigurasi)
- [Endpoints](#endpoints)
- [Skenario Demo](#skenario-demo)
- [Penjelasan Kode](#penjelasan-kode)
- [Troubleshooting](#troubleshooting)

---

## Prasyarat

- Go 1.24+
- Redis (single-instance atau cluster) yang bisa diakses dari mesin demo

---

## Cara Menjalankan

```bash
cd demo/circuit-breaker
go run main.go
```

Output saat startup:

```
✅ Redis connected
✅ Circuit breaker ready (failure_threshold=3, close_threshold=2, allow_after=15s)
🚀 Demo server starting on :3000
```

---

## Konfigurasi

Semua konfigurasi ada di bagian `const` pada `main.go`. Ubah sesuai kebutuhan
sebelum menjalankan:

```go
const (
    // Target external API
    externalAPIURL = "https://jsonplaceholder.typicode.com/posts/1"

    // Redis
    redisClusterToggle      = false           // true = cluster, false = single-instance
    redisSingleInstanceAddr = "localhost:6379"
    redisClusterAddr        = "clustercfg.xxx.cache.amazonaws.com:6379"
    redisDB                 = 1               // hanya untuk single-instance

    // Circuit breaker thresholds
    failureThreshold   = 3   // trip ke OPEN setelah >3 consecutive failures
    closeThreshold     = 2   // kembali ke CLOSED setelah 2 successes di HALF-OPEN
    allowAfterSeconds  = 15  // OPEN → HALF-OPEN setelah 15 detik
)
```

---

## Endpoints

| Method | Path | Fungsi |
|---|---|---|
| `GET` | `/post` | Panggil external API dengan proteksi circuit breaker |
| `GET` | `/state` | Lihat state CB saat ini dari Redis (CLOSED / OPEN / HALF-OPEN) |
| `POST` | `/config` | Ubah target URL saat runtime (untuk simulasi failure) |
| `POST` | `/reset` | Reset state CB di Redis dan kembalikan URL ke semula |

---

## Skenario Demo

### Persiapan

Buka 2 terminal:
- Terminal 1: jalankan server (`go run main.go`)
- Terminal 2: jalankan curl commands

### Step 1 — Normal Flow (CLOSED)

```bash
# Panggil API — harus sukses
curl -s localhost:3000/post | jq .

# Cek state
curl -s localhost:3000/state | jq .
# → state: 0 (CLOSED), consecutive_failures: 0
```

### Step 2 — Simulasi Failure

Arahkan target ke host yang tidak ada:

```bash
curl -s -X POST localhost:3000/config \
  -H "Content-Type: application/json" \
  -d '{"url": "http://localhost:9999"}'
```

### Step 3 — Trigger OPEN

Hit API 4+ kali (threshold = 3, trip setelah >3 failures):

```bash
curl -s localhost:3000/post | jq .error
curl -s localhost:3000/post | jq .error
curl -s localhost:3000/post | jq .error
curl -s localhost:3000/post | jq .error
```

Di terminal server akan muncul:
```
❌ Request failed: request failed: ...
⚡ CB STATE CHANGE [external-api]: Closed → Open
```

### Step 4 — Fast-Fail (OPEN)

```bash
# Request ini langsung ditolak TANPA memanggil external API
curl -s localhost:3000/post | jq .error
# → "circuit breaker is open"

# Cek state
curl -s localhost:3000/state | jq .
# → state: 1, state_name: "OPEN"
```

### Step 5 — Tunggu Transisi ke HALF-OPEN

Tunggu 15 detik (`allowAfterSeconds`), lalu kembalikan URL:

```bash
# Tunggu 15 detik...
sleep 15

# Kembalikan URL ke yang benar
curl -s -X POST localhost:3000/reset | jq .
```

### Step 6 — Recovery (HALF-OPEN → CLOSED)

```bash
# Request pertama — CB izinkan untuk test (HALF-OPEN)
curl -s localhost:3000/post | jq .

# Request kedua — consecutive success = 2 = closeThreshold → CLOSED
curl -s localhost:3000/post | jq .
```

Di terminal server:
```
⚡ CB STATE CHANGE [external-api]: Open → HalfOpen
✅ Request success
⚡ CB STATE CHANGE [external-api]: HalfOpen → Closed
✅ Request success
```

### Step 7 — Verifikasi

```bash
curl -s localhost:3000/state | jq .
# → state: 0, state_name: "CLOSED"
```

---

## Penjelasan Kode

### Struktur

Seluruh demo ada dalam satu file `main.go` dengan bagian-bagian:

| Bagian | Fungsi |
|---|---|
| Config (const) | Konfigurasi Redis, external API URL, dan threshold CB |
| Custom Redis Backend | `redisBackend` struct yang mengimplementasikan `circuitry.StorageBackender` |
| Circuit Breaker Factory | `newCBFactory()` membuat factory dari config |
| External API Call | `fetchPost()` — HTTP GET ke external API |
| Generic Execute Helper | `cbExecute[T]()` — wrapper type-safe untuk CB protection |
| Fiber Handlers | 4 endpoint: `/post`, `/state`, `/config`, `/reset` |

### Mengapa Custom Redis Backend?

Library `circuitry` punya Redis backend bawaan, tapi ada bug: method `Lock()`, `Store()`,
dan `Retrieve()` menggunakan Redis key yang sama. `redislock` menyimpan random token ke key
tersebut, lalu `Retrieve()` mencoba `json.Unmarshal` pada token → error.

Solusi: custom backend yang memisahkan key:
- Lock: `{cb}:lock:<name>`
- Data: `{cb}:data:<name>`

### Mengapa Hash Tag `{cb}`?

Pada Redis Cluster, key di-distribute ke slot berbeda. `redislock` menggunakan Lua script
yang membutuhkan semua key di satu node. Hash tag `{cb}` memastikan semua key CB berada
di slot yang sama, karena Redis Cluster menghitung slot hanya dari konten di dalam `{}`.

### Flow per Request

```
GET /post
  │
  ├─ cbFactory.BreakerFor("external-api")
  │
  ├─ cbExecute(ctx, breaker, fn)
  │    │
  │    ├─ breaker.Start(ctx)
  │    │    ├─ Lock Redis key: {cb}:lock:external-api
  │    │    ├─ GET {cb}:data:external-api → deserialize state
  │    │    └─ Cek state: CLOSED? → lanjut. OPEN? → return error.
  │    │
  │    ├─ fn() → fetchPost(ctx, apiURL)
  │    │    └─ HTTP GET ke external API
  │    │
  │    └─ breaker.End(ctx, workErr)
  │         ├─ Update counter (success/failure)
  │         ├─ SET {cb}:data:external-api → serialize state
  │         └─ Unlock Redis key
  │
  └─ Return response
```

---

## Troubleshooting

| Error | Penyebab | Solusi |
|---|---|---|
| `redis connection failed` | Redis tidak berjalan atau alamat salah | Pastikan Redis aktif, cek `redisAddr` di config |
| `redislock: not obtained` | Lock contention tinggi | Sudah di-handle dengan retry strategy. Jika masih terjadi, naikkan `lockTTLSeconds` |
| `SELECT is not allowed in cluster mode` | `redisClusterToggle=true` tapi `redisDB > 0` | Saat cluster mode, `redisDB` diabaikan. Pastikan `redisClusterToggle` sesuai |
| `circuit breaker is open` | CB sedang di state OPEN | Tunggu `allowAfterSeconds` detik, lalu pastikan external API sudah pulih |
