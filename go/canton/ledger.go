package canton

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// maxFallbackRecords bounds the in-process fallback records.
	maxFallbackRecords = 4096
	// maxPendingReleases bounds the claims and locks remembered for release.
	maxPendingReleases = 16384
	// reconcileLogEvery is how often a release or a record that is still pending is logged.
	reconcileLogEvery = 30 * time.Second
	// ambiguousRetries is how often a Lock or Claim whose reply was lost is tried.
	ambiguousRetries = 2
)

// errRecordConflict means a different payment already owns the hash or the
// updateId: the gate refuses to overwrite it and fails closed.
var errRecordConflict = errors.New("a different payment is already recorded under this hash")

// ledger is the gate's view of the Store: locks and claims that survive an
// ambiguous reply, records that are never overwritten by a different payment,
// and a small in-process fallback so a failed Store does not lose the fact that
// a payment was attempted or settled.
type ledger struct {
	store  Store
	logger *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	fallback map[string]fallbackRecord
	// claims holds claims this process took and gave up (token by updateId): a
	// claim whose reply was lost, or whose release failed. They are not a
	// delivery. The reconciler keeps trying to delete them, and a retry on this
	// process takes them back at once.
	claims *pendingTable
	// locks holds locks whose release failed (tokens by hash).
	locks *pendingTable

	lastRecordLog time.Time

	// work counts paid requests in flight and post-response work; Close waits
	// for it to reach zero. A mutex and a channel, not a WaitGroup: requests may
	// start and finish while Close waits.
	workMu  sync.Mutex
	work    int
	idle    chan struct{}
	closing bool // Close has begun: paid requests that arrive now are refused
}

// enterRequest counts a request to a paid route. Once Close has begun it refuses
// one that carries a payment: the gate was swapped out, and nothing would
// reconcile what that request leaves. The unpaid 402 offer is still answered.
func (l *ledger) enterRequest(carriesPayment bool) bool {
	l.workMu.Lock()
	defer l.workMu.Unlock()
	if l.closing && carriesPayment {
		return false
	}
	l.work++
	return true
}

// beginClose refuses new paid requests and returns the channel closed at zero work.
func (l *ledger) beginClose() <-chan struct{} {
	l.workMu.Lock()
	l.closing = true
	l.workMu.Unlock()
	return l.drained()
}

func (l *ledger) enter() {
	l.workMu.Lock()
	l.work++
	l.workMu.Unlock()
}

func (l *ledger) exit() {
	l.workMu.Lock()
	l.work--
	if l.work == 0 && l.idle != nil {
		close(l.idle)
		l.idle = nil
	}
	l.workMu.Unlock()
}

// drained is closed once no paid request and no post-response work is left.
func (l *ledger) drained() <-chan struct{} {
	l.workMu.Lock()
	defer l.workMu.Unlock()
	if l.work == 0 {
		done := make(chan struct{})
		close(done)
		return done
	}
	if l.idle == nil {
		l.idle = make(chan struct{})
	}
	return l.idle
}

type fallbackRecord struct {
	record  Record
	expires time.Time
}

// pendingRelease is a lock or claim of this process that is still in the Store
// although the request that took it is over. It is released as soon as the Store
// answers, or lapses with its own TTL.
type pendingRelease struct {
	token   string
	expires time.Time
	nextLog time.Time
}

func newLedger(store Store, logger *slog.Logger) *ledger {
	return &ledger{store: store, logger: logger, now: time.Now,
		fallback: make(map[string]fallbackRecord),
		claims:   newPendingTable(), locks: newPendingTable()}
}

// lock takes the lock. When the Store answers with an error the lock may still
// have been applied (a lost reply), so the holder is read back: our token means
// we hold it, an absent key means try again, and when the holder cannot be read
// the lock is released (in the background if it must be) so it does not block
// the payer's retry. A lock of this process that could not be released earlier
// is taken back.
func (l *ledger) lock(ctx context.Context, hash, token string, ttl time.Duration) (bool, error) {
	var lastErr error
	for attempt := 0; attempt < ambiguousRetries; attempt++ {
		acquired, err := l.store.Lock(ctx, hash, token, ttl)
		switch {
		case err == nil && acquired:
			return true, nil
		case err == nil && attempt == 0 && !l.hasPendingLock(hash):
			// "Taken" may be our own write whose reply was lost and re-sent by the
			// client library: a holder that carries our token means we hold it.
			holder, found, readErr := l.store.LockHolder(ctx, hash)
			if readErr != nil {
				l.giveUpLock(ctx, hash, token, ttl) // ours may have applied: compare-and-delete it
				return false, readErr
			}
			return found && holder == token, nil
		case err == nil: // "taken" after an ambiguous first try, or by a lock of ours that was left behind
			holder, found, readErr := l.store.LockHolder(ctx, hash)
			if readErr != nil {
				l.giveUpLock(ctx, hash, token, ttl)
				return false, readErr
			}
			if found && holder != token && l.isPendingLock(hash, holder) {
				if unlockErr := l.store.Unlock(ctx, hash, holder); unlockErr != nil {
					return false, unlockErr // still held: the reconciler keeps trying
				}
				l.forgetLock(hash, holder)
				continue
			}
			return found && holder == token, nil
		case errors.Is(err, errMemoryStoreFull):
			return false, err
		}
		lastErr = err
		holder, found, readErr := l.store.LockHolder(ctx, hash)
		if readErr != nil {
			l.giveUpLock(ctx, hash, token, ttl)
			return false, errors.Join(err, readErr)
		}
		if found {
			return holder == token, nil
		}
	}
	l.giveUpLock(ctx, hash, token, ttl)
	return false, lastErr
}

// giveUpLock releases a lock that may or may not have been applied.
func (l *ledger) giveUpLock(ctx context.Context, hash, token string, ttl time.Duration) {
	l.unlock(ctx, hash, token, ttl)
}

// unlock releases the lock. If the Store cannot do it, the release is retried
// in the background until it succeeds or the lock's own TTL ends, so a retry of
// the payment is not held at "payment in progress" for the whole TTL.
func (l *ledger) unlock(ctx context.Context, hash, token string, ttl time.Duration) {
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	if err := l.store.Unlock(storeCtx, hash, token); err != nil {
		l.rememberLock(hash, token, ttl)
		l.logger.ErrorContext(ctx, "store unlock failed: retried in the background",
			slog.String("canonicalHash", hash), slog.String("error", err.Error()))
	}
}

// errClaimHeld means a claim of another request is on the updateId and is not
// confirmed delivered: its owner may be mid-delivery, or may have given up. The
// gate answers 503 and the payer retries; it is never the final 402.
var errClaimHeld = errors.New("the updateId is claimed by a request that has not confirmed its delivery")

// claim takes the delivery claim. A claim whose reply was lost is resolved by
// reading the holder. When it cannot be resolved the claim is released, so
// nothing of ours is left behind a withheld delivery; if even that fails the
// token is remembered, and the claim is taken back when the payment is retried.
// Only a claim marked delivered is answered as already delivered (false, nil).
func (l *ledger) claim(ctx context.Context, updateID, token string) (bool, error) {
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	var lastErr error
	ambiguous := false
	for attempt := 0; attempt < ambiguousRetries+1; attempt++ {
		claimed, err := l.store.Claim(storeCtx, updateID, token, recordTTL)
		if err == nil && claimed {
			return true, nil
		}
		if err != nil {
			ambiguous, lastErr = true, err
		}
		holder, found, readErr := l.store.Claimant(storeCtx, updateID)
		switch {
		case readErr != nil && ambiguous:
			l.giveUpClaim(ctx, updateID, token)
			return false, errors.Join(err, readErr)
		case readErr != nil:
			// "Taken" by a holder that cannot be read: not the final answer. The gate
			// withholds and the payer retries.
			return false, readErr
		case found && holder == token:
			return true, nil
		case found && isDeliveredMark(holder):
			return false, nil // confirmed delivered: the final answer
		case found && l.isWithheld(updateID, holder):
			// A claim this process gave up earlier is not a delivery: take it back.
			if unclaimErr := l.store.Unclaim(storeCtx, updateID, holder); unclaimErr != nil {
				return false, unclaimErr
			}
			l.forgetClaim(updateID, holder)
		case found:
			// A claim of another request, not confirmed delivered. Read once more: it may be
			// a claim of ours the reconciler has just released and forgotten.
			again, foundAgain, againErr := l.store.Claimant(storeCtx, updateID)
			if againErr != nil {
				return false, againErr
			}
			if foundAgain && again == holder {
				return false, errClaimHeld
			}
		}
		// Not found (a lost reply that did not apply) or taken back: try again.
	}
	l.giveUpClaim(ctx, updateID, token)
	if lastErr == nil {
		lastErr = errors.New("claim could not be resolved")
	}
	return false, lastErr
}

// markDelivered records that the response reached the client. It is called off
// the response path; if it fails the claim stays unmarked, which only makes
// replays answer 503 instead of 402.
func (l *ledger) markDelivered(ctx context.Context, updateID, token string) {
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	if err := l.store.MarkDelivered(storeCtx, updateID, token, recordTTL); err != nil {
		l.logger.ErrorContext(ctx, "delivery not marked: replays answer 503 until the claim lapses",
			slog.String("updateId", updateID), slog.String("error", err.Error()))
	}
}

func (l *ledger) giveUpClaim(ctx context.Context, updateID, token string) {
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	if err := l.store.Unclaim(storeCtx, updateID, token); err != nil {
		stored := l.rememberClaim(updateID, token, recordTTL)
		l.logger.ErrorContext(ctx, "claim could not be released: retried in the background, taken back on retry",
			slog.String("updateId", updateID), slog.Bool("remembered", stored), slog.String("error", err.Error()))
	}
}

// unclaim gives a claim back after a failed write. If the Store cannot do it,
// the token is remembered so a retry on this process can take the claim back.
func (l *ledger) unclaim(ctx context.Context, updateID, token string) {
	l.giveUpClaim(ctx, updateID, token)
}

// claimOwnership says what the claim on an updateId means for a retry.
type claimOwnership int

const (
	// claimNone: no claim, or one this process gave up: nothing was delivered.
	claimNone claimOwnership = iota
	// claimDelivered: a claim marked delivered: the final 402.
	claimDelivered
	// claimHeld: a claim of another request without the delivered mark.
	claimHeld
)

// claimState reads the claim on an updateId. A holder this process does not
// know is confirmed by a second read: the reconciler deletes a given-up claim in
// the Store before it forgets the token, so a holder read just before that delete
// must not be taken for a delivery.
func (l *ledger) claimState(ctx context.Context, updateID string) (claimOwnership, error) {
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	for attempt := 0; attempt < ambiguousRetries; attempt++ {
		holder, found, err := l.store.Claimant(storeCtx, updateID)
		switch {
		case err != nil:
			return claimNone, err
		case !found || l.isWithheld(updateID, holder):
			return claimNone, nil
		case isDeliveredMark(holder):
			return claimDelivered, nil
		}
		again, foundAgain, err := l.store.Claimant(storeCtx, updateID)
		switch {
		case err != nil:
			return claimNone, err
		case !foundAgain:
			return claimNone, nil
		case again != holder:
			continue // changed between the reads: classify the new holder
		case l.isWithheld(updateID, again):
			return claimNone, nil
		}
		return claimHeld, nil
	}
	return claimHeld, nil
}

// lookup returns the record of a payment. The in-process fallback holds writes
// the Store refused; a settled record beats an attempt, and the Store wins a tie.
func (l *ledger) lookup(ctx context.Context, hash string) (Record, bool, error) {
	fallback, inFallback := l.fromFallback(hash)
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	record, found, err := l.store.Record(storeCtx, hash)
	if !inFallback {
		return record, found, err
	}
	if err != nil || !found || (fallback.State == StateSettled && record.State != StateSettled) {
		return fallback, true, nil
	}
	if record.State == StateSettled {
		l.dropFromFallback(hash)
	}
	return record, true, nil
}

// saveAttempt records that /settle is about to run. An existing record is kept:
// an attempt never downgrades a settled payment or replaces another payload's.
// When the Store cannot even be read, the attempt is kept in process and still
// written to the Store if it will take it.
func (l *ledger) saveAttempt(ctx context.Context, hash string, record Record) error {
	_, found, err := l.lookup(ctx, hash)
	if err != nil {
		l.toFallback(ctx, hash, record)
		_ = l.write(ctx, hash, record)
		return err
	}
	if found {
		return nil
	}
	return l.write(ctx, hash, record)
}

// saveSettled records the settled payment. It refuses to overwrite a settled
// record of a different updateId. A settled record of the same updateId is kept
// as it is (its digest may come from another /supported pin: the facilitator
// confirmed the transaction either way). An attempt is replaced by the settled
// record. When the Store cannot be read the record is kept in process (and
// written if the Store takes it), and the error is returned: the caller
// withholds the delivery, and the retry is served.
func (l *ledger) saveSettled(ctx context.Context, hash string, record Record) error {
	existing, found, err := l.lookup(ctx, hash)
	if err != nil {
		l.toFallback(ctx, hash, record)
		_ = l.write(ctx, hash, record)
		return err
	}
	if found && existing.State == StateSettled {
		if existing.UpdateID != record.UpdateID {
			return errRecordConflict
		}
		return nil
	}
	return l.write(ctx, hash, record)
}

// write stores the record; when the Store refuses, the record is kept in the
// fallback so a retry in this process still sees it.
func (l *ledger) write(ctx context.Context, hash string, record Record) error {
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	err := l.store.PutRecord(storeCtx, hash, record, recordTTL)
	if err != nil {
		l.toFallback(ctx, hash, record)
		return err
	}
	l.dropFromFallback(hash)
	return nil
}

// clear removes an attempt record after a definite refusal, while the request
// still holds the payment's lock. If the Store cannot delete it the attempt
// stays until its TTL: a retry of those bytes then skips /verify, and a handler
// refusal is answered 503 instead of passed through (documented).
func (l *ledger) clear(ctx context.Context, hash string) {
	l.dropFromFallback(hash)
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	if err := l.store.DeleteRecord(storeCtx, hash); err != nil {
		l.logger.ErrorContext(ctx, "attempt record could not be cleared: it stays until its TTL",
			slog.String("canonicalHash", hash), slog.String("error", err.Error()))
	}
}

func (l *ledger) fromFallback(hash string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, found := l.fallback[hash]
	if !found || !l.now().Before(entry.expires) {
		delete(l.fallback, hash)
		return Record{}, false
	}
	return entry.record, true
}

func (l *ledger) toFallback(ctx context.Context, hash string, record Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, replacing := l.fallback[hash]; !replacing && len(l.fallback) >= maxFallbackRecords {
		for key, entry := range l.fallback {
			if !l.now().Before(entry.expires) {
				delete(l.fallback, key)
			}
		}
		if len(l.fallback) >= maxFallbackRecords {
			l.logger.ErrorContext(ctx, "fallback record table is full: payment not remembered in process", slog.String("canonicalHash", hash))
			return
		}
	}
	l.fallback[hash] = fallbackRecord{record: record, expires: l.now().Add(recordTTL)}
}

func (l *ledger) dropFromFallback(hash string) {
	l.mu.Lock()
	delete(l.fallback, hash)
	l.mu.Unlock()
}

func (l *ledger) rememberClaim(updateID, token string, ttl time.Duration) bool {
	return l.remember(l.claims, updateID, token, ttl)
}

// pendingTable keeps every token remembered for a key: a second give-up for the
// same key must not make the first one forgotten while it is still in the Store.
type pendingTable struct {
	byKey map[string]map[string]pendingRelease
	size  int
}

func newPendingTable() *pendingTable {
	return &pendingTable{byKey: make(map[string]map[string]pendingRelease)}
}

func (t *pendingTable) get(key, token string) (pendingRelease, bool) {
	entry, found := t.byKey[key][token]
	return entry, found
}

func (t *pendingTable) put(key, token string, entry pendingRelease) {
	tokens := t.byKey[key]
	if tokens == nil {
		tokens = make(map[string]pendingRelease)
		t.byKey[key] = tokens
	}
	if _, replacing := tokens[token]; !replacing {
		t.size++
	}
	tokens[token] = entry
}

func (t *pendingTable) del(key, token string) {
	tokens := t.byKey[key]
	if _, found := tokens[token]; !found {
		return
	}
	delete(tokens, token)
	t.size--
	if len(tokens) == 0 {
		delete(t.byKey, key)
	}
}

func (l *ledger) rememberLock(hash, token string, ttl time.Duration) {
	if !l.remember(l.locks, hash, token, ttl) {
		l.logger.Error("lock release could not be remembered: the lock lapses with its TTL", slog.String("canonicalHash", hash))
	}
}

// remember stores a pending release. It reports whether it was stored.
func (l *ledger) remember(table *pendingTable, key, token string, ttl time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if _, replacing := table.get(key, token); !replacing && table.size >= maxPendingReleases {
		for k, tokens := range table.byKey {
			for tok, entry := range tokens {
				if !now.Before(entry.expires) {
					table.del(k, tok)
				}
			}
		}
		if table.size >= maxPendingReleases {
			return false
		}
	}
	table.put(key, token, pendingRelease{token: token, expires: now.Add(ttl), nextLog: now.Add(reconcileLogEvery)})
	return true
}

func (l *ledger) isWithheld(updateID, token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, found := l.claims.get(updateID, token)
	return found && l.now().Before(entry.expires)
}

func (l *ledger) isPendingLock(hash, token string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, found := l.locks.get(hash, token)
	return found && l.now().Before(entry.expires)
}

func (l *ledger) hasPendingLock(hash string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks.byKey[hash]) > 0
}

// forgetClaim forgets a pending claim only if it still holds that token.
func (l *ledger) forgetClaim(updateID, token string) {
	l.mu.Lock()
	l.claims.del(updateID, token)
	l.mu.Unlock()
}

func (l *ledger) forgetLock(hash, token string) {
	l.mu.Lock()
	l.locks.del(hash, token)
	l.mu.Unlock()
}

// run is the background reconciler. Until ctx ends it retries, with the Store,
// what a Store failure left behind in this process: claims and locks to delete,
// and records that exist only in the fallback. Each item is retried until it
// succeeds or its TTL ends. The updateIds still pending are logged at error.
func (l *ledger) run(ctx context.Context, interval time.Duration, logOnStop bool) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if logOnStop {
		defer l.logLeftBehind()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			l.reconcileGuarded(ctx)
		}
	}
}

func (l *ledger) reconcileGuarded(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			l.logger.Error("store panicked in the background reconciler", slog.Any("panic", r))
		}
	}()
	l.reconcileOnce(ctx)
}

func (l *ledger) logLeftBehind() {
	l.mu.Lock()
	claims, locks, records := l.claims.size, l.locks.size, len(l.fallback)
	l.mu.Unlock()
	if claims+locks+records > 0 {
		l.logger.Error("background reconciler stopped with work left: these are not released by this process",
			slog.Int("claims", claims), slog.Int("locks", locks), slog.Int("records", records))
	}
}

// reconcileContext bounds one reconciler Store call and, unlike storeContext,
// ends with the reconciler's own context.
func reconcileContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, storeCallTimeout)
}

type pendingItem struct {
	key, token string
	log        bool
}

func (l *ledger) snapshot(table *pendingTable) []pendingItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	items := make([]pendingItem, 0, table.size)
	for key, tokens := range table.byKey {
		for token, entry := range tokens {
			if !now.Before(entry.expires) {
				table.del(key, token)
				continue
			}
			due := !now.Before(entry.nextLog)
			if due {
				entry.nextLog = now.Add(reconcileLogEvery)
				tokens[token] = entry
			}
			items = append(items, pendingItem{key: key, token: token, log: due})
		}
	}
	return items
}

func (l *ledger) reconcileOnce(ctx context.Context) {
	for _, item := range l.snapshot(l.claims) {
		if ctx.Err() != nil {
			return
		}
		storeCtx, cancel := reconcileContext(ctx)
		err := l.store.Unclaim(storeCtx, item.key, item.token)
		cancel()
		switch {
		case err == nil:
			l.forgetClaim(item.key, item.token)
		case item.log:
			l.logger.ErrorContext(ctx, "claim still not released: payment not served until the Store answers",
				slog.String("updateId", item.key), slog.String("error", err.Error()))
		}
	}
	for _, item := range l.snapshot(l.locks) {
		if ctx.Err() != nil {
			return
		}
		storeCtx, cancel := reconcileContext(ctx)
		err := l.store.Unlock(storeCtx, item.key, item.token)
		cancel()
		switch {
		case err == nil:
			l.forgetLock(item.key, item.token)
		case item.log:
			l.logger.ErrorContext(ctx, "lock still not released", slog.String("canonicalHash", item.key), slog.String("error", err.Error()))
		}
	}
	l.reconcileRecords(ctx)
}

// reconcileRecords writes records that exist only in the fallback to the Store.
func (l *ledger) reconcileRecords(ctx context.Context) {
	l.mu.Lock()
	now := l.now()
	pending := make(map[string]Record, len(l.fallback))
	for hash, entry := range l.fallback {
		if now.Before(entry.expires) {
			pending[hash] = entry.record
		} else {
			delete(l.fallback, hash)
		}
	}
	l.mu.Unlock()
	for hash, record := range pending {
		if ctx.Err() != nil {
			return
		}
		err := l.reconcileRecord(ctx, hash, record)
		switch {
		case err == nil || errors.Is(err, errReconcileBusy):
		case l.recordLogDue():
			l.logger.ErrorContext(ctx, "payment record still only in process memory",
				slog.String("canonicalHash", hash), slog.String("state", string(record.State)), slog.String("updateId", record.UpdateID),
				slog.String("error", err.Error()))
		}
	}
}

// errReconcileBusy: a request holds the payment; its own hooks write the record.
var errReconcileBusy = errors.New("payment in progress")

// reconcileLockTTL bounds the reconciler's hold on one payment.
const reconcileLockTTL = 3 * storeCallTimeout

// reconcileRecord writes one fallback record under the payment's own lock, so a
// request that settles, refuses or clears the same payment cannot interleave,
// and drops the fallback entry only if it is still the record it wrote.
func (l *ledger) reconcileRecord(ctx context.Context, hash string, record Record) error {
	storeCtx, cancel := reconcileContext(ctx)
	defer cancel()
	token := newToken()
	acquired, err := l.store.Lock(storeCtx, hash, token, reconcileLockTTL)
	if err != nil {
		l.unlock(ctx, hash, token, reconcileLockTTL) // it may have applied
		return err
	}
	if !acquired {
		return errReconcileBusy
	}
	defer l.unlock(ctx, hash, token, reconcileLockTTL)
	if current, still := l.fromFallback(hash); !still || current != record {
		return nil // the request path changed it since the snapshot: next pass
	}
	stored, found, err := l.store.Record(storeCtx, hash)
	if err != nil {
		return err
	}
	if !found || (stored.State != StateSettled && record.State == StateSettled) {
		if err := l.store.PutRecord(storeCtx, hash, record, recordTTL); err != nil {
			return err
		}
	}
	l.dropFromFallbackIf(hash, record)
	return nil
}

func (l *ledger) dropFromFallbackIf(hash string, record Record) {
	l.mu.Lock()
	if entry, found := l.fallback[hash]; found && entry.record == record {
		delete(l.fallback, hash)
	}
	l.mu.Unlock()
}

func (l *ledger) recordLogDue() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); !now.Before(l.lastRecordLog.Add(reconcileLogEvery)) {
		l.lastRecordLog = now
		return true
	}
	return false
}

// errNilRedisClient is a configuration error, never retried.
var errNilRedisClient = errors.New("NewRedisStore was given a nil RedisClient")

// probe locks and unlocks a throwaway key at startup, so an unusable Store (an
// unreachable Redis, a read-only replica, an ACL that refuses SET NX or the Lua
// compare-and-delete, a nil client) is a startup error and not a surprise for the
// first payer.
func (l *ledger) probe(ctx context.Context) error {
	if redis, isRedis := l.store.(*redisStore); isRedis && redis.client == nil {
		return errNilRedisClient
	}
	storeCtx, cancel := storeContext(ctx)
	defer cancel()
	if checker, ok := l.store.(interface{ startupCheck(context.Context) error }); ok {
		if err := checker.startupCheck(storeCtx); err != nil {
			return err
		}
	}
	token := newToken()
	hash := strings.Repeat("0", 64-len(token)) + token // the canonical hash shape
	if _, _, err := l.store.Record(storeCtx, hash); err != nil {
		return err
	}
	if _, err := l.store.Lock(storeCtx, hash, token, probeLockTTL); err != nil {
		if errors.Is(err, errMemoryStoreFull) {
			return nil // usable, only full: the gate refuses payments before settling, as designed
		}
		return err
	}
	return l.store.Unlock(storeCtx, hash, token)
}

// probeLockTTL lets a probe key that Unlock did not remove vanish quickly.
const probeLockTTL = 30 * time.Second

// background runs fn on its own goroutine and waits for it at most wait, so a
// slow Store cannot hold a response open. Close waits for what is left.
func (l *ledger) background(fn func(), wait time.Duration) {
	done := make(chan struct{})
	l.enter()
	go func() {
		defer l.exit()
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				l.logger.Error("store panicked in post-response work", slog.Any("panic", r))
			}
		}()
		fn()
	}()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}
