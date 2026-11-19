package echo_test

import "runtime"

type runtimeMem struct{ total uint64 }

func (m *runtimeMem) read() {
	runtime.GC()
	var s runtime.MemStats
	runtime.ReadMemStats(&s)
	m.total = s.TotalAlloc
}
