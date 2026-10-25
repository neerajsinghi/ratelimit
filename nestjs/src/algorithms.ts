// Pure implementations of the rate limiting algorithms.
//
// These mirror scripts/gcra.lua and scripts/sliding_window.lua (and the Go
// module's algorithm.go) line for line. JavaScript numbers are IEEE doubles,
// exactly like Lua 5.1 in Redis, so the results are bit-for-bit identical.
// test/parity.spec.ts checks this against the shared testdata/vectors.json.

/** A decision in integer microseconds, as returned by the Lua scripts. */
export interface RawResult {
  allowed: boolean;
  remaining: number;
  retryAfterUs: number;
  resetAfterUs: number;
}

/** Per-key GCRA state: the theoretical arrival time in microseconds. */
export interface GcraState {
  tat: number;
}

/** Per-key sliding-window state. */
export interface SlidingWindowState {
  window: number;
  cur: number;
  prev: number;
}

export interface Step<S> {
  result: RawResult;
  /** New state to store, or undefined if nothing changed. */
  state?: S;
  /** Time to live for the stored state, in microseconds. */
  ttlUs: number;
}

/** One GCRA decision. All times are microseconds. */
export function gcraStep(
  limit: number,
  period: number,
  cost: number,
  now: number,
  st: GcraState | undefined,
): Step<GcraState> {
  const interval = Math.floor(period / limit);
  const burst = interval * limit;

  let tat = st?.tat;
  if (tat === undefined || tat < now) {
    tat = now;
  }

  const newTat = tat + interval * cost;
  const diff = now - (newTat - burst);

  if (diff < 0) {
    let remaining = Math.floor((now - (tat - burst)) / interval);
    if (remaining < 0) {
      remaining = 0;
    }
    return {
      result: { allowed: false, remaining, retryAfterUs: -diff, resetAfterUs: tat - now },
      ttlUs: 0,
    };
  }

  const resetAfter = newTat - now;
  return {
    result: {
      allowed: true,
      remaining: Math.floor(diff / interval),
      retryAfterUs: 0,
      resetAfterUs: resetAfter,
    },
    state: cost > 0 ? { tat: newTat } : undefined,
    ttlUs: resetAfter,
  };
}

/** One sliding-window counter decision. All times are microseconds. */
export function slidingWindowStep(
  limit: number,
  period: number,
  cost: number,
  now: number,
  st: SlidingWindowState | undefined,
): Step<SlidingWindowState> {
  const window = Math.floor(now / period);
  const elapsed = now - window * period;

  let c = 0;
  let p = 0;
  if (st?.window === window) {
    c = st.cur;
    p = st.prev;
  } else if (st?.window === window - 1) {
    p = st.cur;
  }

  const estimate = (p * (period - elapsed)) / period + c;

  const resetAfter = (cur: number, prev: number): number => {
    if (cur > 0) {
      return period - elapsed + period;
    } else if (prev > 0) {
      return period - elapsed;
    }
    return 0;
  };

  if (estimate + cost <= limit) {
    const remaining = Math.floor(limit - estimate - cost);
    c = c + cost;
    return {
      result: { allowed: true, remaining, retryAfterUs: 0, resetAfterUs: resetAfter(c, p) },
      state: cost > 0 ? { window, cur: c, prev: p } : undefined,
      ttlUs: period - elapsed + period,
    };
  }

  let remaining = Math.floor(limit - estimate);
  if (remaining < 0) {
    remaining = 0;
  }

  let retryAfter: number;
  if (c + cost <= limit) {
    retryAfter = Math.ceil(period - ((limit - c - cost) * period) / p) - elapsed;
  } else {
    retryAfter = period - elapsed;
    const extra = Math.ceil(period - ((limit - cost) * period) / c);
    if (extra > 0) {
      retryAfter = retryAfter + extra;
    }
  }

  return {
    result: { allowed: false, remaining, retryAfterUs: retryAfter, resetAfterUs: resetAfter(c, p) },
    ttlUs: 0,
  };
}
