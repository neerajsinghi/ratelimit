package ratelimit

import "math"

// This file mirrors scripts/gcra.lua and scripts/sliding_window.lua line for
// line. Keep them in sync: TestParity runs both against shared vectors.

// raw is a limiter decision in integer microseconds, as returned by the Lua
// scripts.
type raw struct {
	allowed    bool
	remaining  int64
	retryAfter int64
	resetAfter int64
}

// floorDiv and ceilDiv divide using float64 like Lua 5.1 does, so that the
// in-memory and Redis implementations agree bit for bit.
func floorDiv(a, b int64) int64 { return int64(math.Floor(float64(a) / float64(b))) }

// gcraState is the per-key state of GCRA: the theoretical arrival time.
type gcraState struct {
	tat int64
}

// gcraStep applies one GCRA decision. ok reports whether state existed.
// It returns the decision, the new state and whether the state must be
// stored (with a TTL of resetAfter).
func gcraStep(limit, period, cost, now int64, st gcraState, ok bool) (raw, gcraState, bool) {
	interval := floorDiv(period, limit)
	burst := interval * limit

	tat := st.tat
	if !ok || tat < now {
		tat = now
	}

	newTat := tat + interval*cost
	diff := now - (newTat - burst)

	if diff < 0 {
		remaining := floorDiv(now-(tat-burst), interval)
		if remaining < 0 {
			remaining = 0
		}
		return raw{remaining: remaining, retryAfter: -diff, resetAfter: tat - now}, st, false
	}

	resetAfter := newTat - now
	return raw{allowed: true, remaining: floorDiv(diff, interval), resetAfter: resetAfter},
		gcraState{tat: newTat}, cost > 0
}

// swState is the per-key state of the sliding-window counter.
type swState struct {
	window int64 // index of the current fixed window
	cur    int64 // count in the current window
	prev   int64 // count in the previous window
}

// swStep applies one sliding-window decision. ok reports whether state
// existed. It returns the decision, the new state, whether it must be stored
// and its TTL in microseconds.
func swStep(limit, period, cost, now int64, st swState, ok bool) (raw, swState, bool, int64) {
	window := floorDiv(now, period)
	elapsed := now - window*period

	var c, p int64
	switch {
	case ok && st.window == window:
		c, p = st.cur, st.prev
	case ok && st.window == window-1:
		p = st.cur
	}

	fl, fp, fc := float64(limit), float64(period), float64(cost)
	estimate := float64(p)*float64(period-elapsed)/fp + float64(c)

	resetAfter := func(cur, prev int64) int64 {
		switch {
		case cur > 0:
			return period - elapsed + period
		case prev > 0:
			return period - elapsed
		}
		return 0
	}

	if estimate+fc <= fl {
		remaining := int64(math.Floor(fl - estimate - fc))
		c += cost
		ttl := (period - elapsed + period)
		return raw{allowed: true, remaining: remaining, resetAfter: resetAfter(c, p)},
			swState{window: window, cur: c, prev: p}, cost > 0, ttl
	}

	remaining := int64(math.Floor(fl - estimate))
	if remaining < 0 {
		remaining = 0
	}

	var retryAfter int64
	if c+cost <= limit {
		retryAfter = int64(math.Ceil(fp-float64(limit-c-cost)*fp/float64(p))) - elapsed
	} else {
		retryAfter = period - elapsed
		extra := int64(math.Ceil(fp - float64(limit-cost)*fp/float64(c)))
		if extra > 0 {
			retryAfter += extra
		}
	}

	return raw{remaining: remaining, retryAfter: retryAfter, resetAfter: resetAfter(c, p)}, st, false, 0
}
