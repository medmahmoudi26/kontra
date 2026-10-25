package config

import "time"

// Defaults from PRD §11 and spec/relay.md §2.2, §5.4, §6.4–§6.6, §11.2, §13.1, §14.2–§14.3, §17.
const (
	DefaultListen         = ":8080"
	DefaultMetricsListen  = ":9090"
	DefaultBody           = 4 << 20
	DefaultInflightPerKey = 32
	DefaultRPSPerIP       = 50
	DefaultHeaderTimeout  = 10 * time.Second
	DefaultIdle           = 60 * time.Second
	DefaultDispatchSlots  = 10000
	DefaultPushSlots      = 2000
	DefaultTaskBytes      = 256 << 20
	DefaultTemporalAddr   = "localhost:7233"
	DefaultNamespace      = "kontra"
	DefaultTaskTimeout    = time.Hour
	DefaultTaskRetries    = 2
	DefaultTaskRetention  = 7 * 24 * time.Hour
	DefaultSessionIdle    = 3600 // seconds
	DefaultDrainTimeout   = 10 * time.Minute
	DefaultAdmissionWait  = 5 * time.Second
)
