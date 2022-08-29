package uploads

import (
	"sync"
	"time"

	config_proto "www.velocidex.com/golang/velociraptor/config/proto"
	"www.velocidex.com/golang/velociraptor/executor"
)

var (
	mu               sync.Mutex
	gUploaderFactory *UploaderFactory
)

// For ElasticIngestor we need to keep track of the row id for uploads
// since the server has no state. Therefore we need to maintain the
// state client side. All uploads within the same collection currently
// go into the same result set - therefore we track uploads per
// session id.
type SessionTracker struct {
	session_id string

	// Total row count
	count int

	// Last upload time.
	age time.Time
}

type UploaderFactory struct {
	config_obj *config_proto.Config
	client_id  string
	exe        *executor.ClientExecutor

	mu              sync.Mutex
	session_tracker map[string]*SessionTracker
}

// Really simple because this is not expected to be very large.
func (self *UploaderFactory) expireTrackers() {
	old := []string{}
	limit := time.Now().Add(-time.Hour)

	for k, v := range self.session_tracker {
		if v.age.Before(limit) {
			old = append(old, k)
		}
	}

	for _, k := range old {
		delete(self.session_tracker, k)
	}
}

func (self *UploaderFactory) GetTracker(session_id string) *SessionTracker {
	self.mu.Lock()
	defer self.mu.Unlock()

	existing, pres := self.session_tracker[session_id]
	if !pres {
		existing = &SessionTracker{
			session_id: session_id,
		}
		self.session_tracker[session_id] = existing
	}

	existing.age = time.Now()
	self.expireTrackers()

	return existing
}

func (self *UploaderFactory) ReturnTracker(tracker *SessionTracker) int {
	self.mu.Lock()
	defer self.mu.Unlock()

	count := tracker.count
	tracker.count++
	tracker.age = time.Now()
	self.session_tracker[tracker.session_id] = tracker

	return count
}

func SetUploaderService(
	config_obj *config_proto.Config,
	client_id string,
	exe *executor.ClientExecutor) {
	mu.Lock()
	defer mu.Unlock()

	gUploaderFactory = &UploaderFactory{
		config_obj:      config_obj,
		exe:             exe,
		client_id:       client_id,
		session_tracker: make(map[string]*SessionTracker),
	}
}
