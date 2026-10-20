package main

import (
	"errors"
	"time"
)

// Queues try a failed download again by themselves: Queue.Retries times, every
// Queue.RetryMins minutes, moving it to the end of the queue each time (as IDM does). While a
// failed download waits for its next try, the queue isn't finished, so "when finished" doesn't
// run early. Errors that trying again can't fix (a missing file, a refused login, a full disk)
// aren't retried. Tries are counted in memory and start over when the download is started by
// hand or finishes.

type retryInfo struct {
	tries int       // tries used
	at    time.Time // tries again then; zero = not waiting
}

// retryTimes is how many times the queue tries a failed download again.
func (q *Queue) retryTimes() int {
	if q.Retries == nil {
		return 3
	}
	return *q.Retries
}

// permanentFailure: trying again later won't help.
func permanentFailure(err error) bool {
	var pe permanentErr
	if errors.As(err, &pe) {
		return true
	}
	var se statusErr
	return errors.As(err, &se) && se.code >= 400 && se.code < 500 && !retryStatus(se.code)
}

// noteFailure is called when a download failed (caller holds m.mu): a download its queue runs
// gets another try later, if the queue allows one more.
func (m *Manager) noteFailure(d *Download, err error) {
	q := m.queueByID(d.Queue)
	if q == nil || !m.qRun[q.ID] || d.Provisional || permanentFailure(err) {
		delete(m.sx.retry, d.ID)
		return
	}
	if m.sx.retry == nil {
		m.sx.retry = map[string]*retryInfo{}
	}
	r := m.sx.retry[d.ID]
	if r == nil {
		r = &retryInfo{}
		m.sx.retry[d.ID] = r
	}
	r.at = time.Time{}
	if r.tries >= q.retryTimes() {
		return // tried enough: it stays failed
	}
	r.tries++
	r.at = time.Now().Add(time.Duration(q.RetryMins) * time.Minute)
	logf("%s: trying again in %d min (%d of %d)", d.FileName, q.RetryMins, r.tries, q.retryTimes())
}

// retryWaiting: d failed and its queue tries it again later. Caller holds m.mu.
func (m *Manager) retryWaiting(d *Download) bool {
	r := m.sx.retry[d.ID]
	return r != nil && !r.at.IsZero() && d.Status == StError
}

// retryAt is when d's queue tries it again (Unix ms), 0 = it doesn't. Caller holds m.mu.
func (m *Manager) retryAt(d *Download) int64 {
	if !m.retryWaiting(d) {
		return 0
	}
	return m.sx.retry[d.ID].at.UnixMilli()
}

// forgetRetry starts the count over (started by hand). Caller holds m.mu.
func (m *Manager) forgetRetry(id string) { delete(m.sx.retry, id) }

// retryTick puts failed downloads whose time has come back in their queue, at the end.
func (m *Manager) retryTick(now time.Time) {
	m.mu.Lock()
	again := false
	for id, r := range m.sx.retry {
		d := m.byID[id]
		switch {
		case d == nil || d.Status == StCompleted:
			delete(m.sx.retry, id) // removed, or done: nothing to count any more
		case r.at.IsZero() || now.Before(r.at):
		case d.Status != StError: // stopped or started again meanwhile
			r.at = time.Time{}
		case !m.qRun[d.Queue]: // the queue was stopped: its next start tries again
			delete(m.sx.retry, id)
		default:
			r.at = time.Time{}
			d.Status, d.Error = StQueued, ""
			m.toEnd(d)
			m.dirty, again = true, true
		}
	}
	m.mu.Unlock()
	if again {
		m.kick()
	}
}

// toEnd moves d to the end of the list, which is also the end of its queue. Caller holds m.mu.
func (m *Manager) toEnd(d *Download) {
	for i, x := range m.list {
		if x == d {
			m.list = append(append(m.list[:i:i], m.list[i+1:]...), d)
			return
		}
	}
}
