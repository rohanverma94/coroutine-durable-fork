package coroutine

import (
	"reflect"
	"testing"
	"unsafe"
)

func TestLocalStorage(t *testing.T) {
	ch := make(chan any)
	key := uintptr(0)
	val := any(42)

	go with(&key, val, func() {
		ch <- load(key)
	})

	if v := <-ch; !reflect.DeepEqual(v, val) {
		t.Errorf("wrong value for key=%v: %v", key, *(*[2]unsafe.Pointer)(unsafe.Pointer(&v)))
	}
}

//go:noinline
func weirdLoop(n int, f func()) int {
	if n == 0 {
		f()
		return 0
	} else {
		return weirdLoop(n-1, f) + 1 // just in case Go ever implements tail recursion
	}
}

func TestLocalStorageGrowStack(t *testing.T) {
	ch := make(chan any)
	key := uintptr(0)
	val := any(42)

	go with(&key, val, func() {
		weirdLoop(100e3, func() { ch <- load(key) })
	})

	if v := <-ch; !reflect.DeepEqual(v, val) {
		t.Errorf("wrong value for key=%v: %v", key, *(*[2]unsafe.Pointer)(unsafe.Pointer(&v)))
	}
}

func BenchmarkLocalStorage(b *testing.B) {
	key := uintptr(0)
	val := any(42)
	with(&key, val, func() {
		for i := 0; i < b.N; i++ {
			load(key)
		}
	})
}
