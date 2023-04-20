package main

import (
	"math/rand"
	"strconv"
	"testing"
	"time"

	"github.com/allegro/bigcache"
	cache "github.com/brainly/echo-http-cache"
	"github.com/brainly/echo-http-cache/adapter/memory"
)

const maxEntrySize = 256

func BenchmarkHTTPCacheMamoryAdapterSet(b *testing.B) {
	cache, expiration := initHTTPCacheMamoryAdapter(b.N)
	for i := 0; i < b.N; i++ {
		err := cache.Set(uint64(i), value(), expiration)
		if err != nil {
			return
		}
	}
}

func BenchmarkBigCacheSet(b *testing.B) {
	cache := initBigCache(b.N)
	for i := 0; i < b.N; i++ {
		err := cache.Set(string(rune(i)), value())
		if err != nil {
			return
		}
	}
}

func BenchmarkHTTPCacheMamoryAdapterGet(b *testing.B) {
	b.StopTimer()
	cache, expiration := initHTTPCacheMamoryAdapter(b.N)
	for i := 0; i < b.N; i++ {
		err := cache.Set(uint64(i), value(), expiration)
		if err != nil {
			return
		}
	}

	b.StartTimer()
	for i := 0; i < b.N; i++ {
		cache.Get(uint64(i))
	}
}

func BenchmarkBigCacheGet(b *testing.B) {
	b.StopTimer()
	cache := initBigCache(b.N)
	for i := 0; i < b.N; i++ {
		err := cache.Set(string(rune(i)), value())
		if err != nil {
			return
		}
	}

	b.StartTimer()
	for i := 0; i < b.N; i++ {
		_, err := cache.Get(string(rune(i)))
		if err != nil {
			return
		}
	}
}

func BenchmarkHTTPCacheMamoryAdapterSetParallel(b *testing.B) {
	cache, expiration := initHTTPCacheMamoryAdapter(b.N)
	rand.Seed(time.Now().Unix())

	b.RunParallel(func(pb *testing.PB) {
		id := rand.Intn(1000)
		counter := 0
		for pb.Next() {
			err := cache.Set(parallelKey(id, counter), value(), expiration)
			if err != nil {
				return
			}
			counter = counter + 1
		}
	})
}

func BenchmarkBigCacheSetParallel(b *testing.B) {
	cache := initBigCache(b.N)
	rand.Seed(time.Now().Unix())

	b.RunParallel(func(pb *testing.PB) {
		id := rand.Intn(1000)
		counter := 0
		for pb.Next() {
			err := cache.Set(strconv.FormatUint(parallelKey(id, counter), 10), value())
			if err != nil {
				return
			}
			counter = counter + 1
		}
	})
}

func BenchmarkHTTPCacheMemoryAdapterGetParallel(b *testing.B) {
	b.StopTimer()
	cache, expiration := initHTTPCacheMamoryAdapter(b.N)
	for i := 0; i < b.N; i++ {
		err := cache.Set(uint64(i), value(), expiration)
		if err != nil {
			return
		}
	}

	b.StartTimer()
	b.RunParallel(func(pb *testing.PB) {
		counter := 0
		for pb.Next() {
			cache.Get(uint64(counter))
			counter = counter + 1
		}
	})
}

func BenchmarkBigCacheGetParallel(b *testing.B) {
	b.StopTimer()
	cache := initBigCache(b.N)
	for i := 0; i < b.N; i++ {
		err := cache.Set(string(rune(i)), value())
		if err != nil {
			return
		}
	}

	b.StartTimer()
	b.RunParallel(func(pb *testing.PB) {
		counter := 0
		for pb.Next() {
			_, err := cache.Get(string(rune(counter)))
			if err != nil {
				return
			}
			counter = counter + 1
		}
	})
}

func value() []byte {
	return make([]byte, 100)
}

func parallelKey(threadID int, counter int) uint64 {
	return uint64(threadID)
}

func initHTTPCacheMamoryAdapter(entries int) (cache.Adapter, time.Time) {
	if entries < 2 {
		entries = 2
	}
	adapter, _ := memory.NewAdapter(
		memory.AdapterWithCapacity(entries),
		memory.AdapterWithAlgorithm(memory.LRU),
	)

	return adapter, time.Now().Add(1 * time.Minute)
}

func initBigCache(entriesInWindow int) *bigcache.BigCache {
	cache, _ := bigcache.NewBigCache(bigcache.Config{
		Shards:             256,
		LifeWindow:         10 * time.Minute,
		MaxEntriesInWindow: entriesInWindow,
		MaxEntrySize:       maxEntrySize,
		Verbose:            true,
	})

	return cache
}
