module github.com/JMjirapat/tokipe/toolcache/redis

go 1.23

replace github.com/JMjirapat/tokipe => ../..

require (
	// v0.0.0-00010101... is the placeholder Go writes for a replaced module.
	// The replace below is ignored downstream, so this must be a real version.
	github.com/JMjirapat/tokipe v1.0.0
	github.com/alicebob/miniredis/v2 v2.33.0
	github.com/redis/go-redis/v9 v9.7.3
)

require (
	github.com/alicebob/gopher-json v0.0.0-20200520072559-a9ecdc9d1d3a // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/yuin/gopher-lua v1.1.1 // indirect
)
