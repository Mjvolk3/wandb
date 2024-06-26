package filestream_test

import (
	"fmt"
	"time"

	"github.com/wandb/wandb/core/internal/api"
	"github.com/wandb/wandb/core/internal/apitest"

	"github.com/wandb/wandb/core/pkg/observability"

	"github.com/wandb/wandb/core/pkg/filestream"

	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/segmentio/encoding/json"

	"sync"

	"github.com/stretchr/testify/assert"
	"github.com/wandb/wandb/core/pkg/service"
)

type captureState struct {
	lock sync.RWMutex
	m    map[string]interface{}
}

func (hs *captureState) inc(s string) {
	if hs.m == nil {
		return
	}
	hs.lock.Lock()
	defer hs.lock.Unlock()
	data, ok := hs.m[s].(int)
	if !ok {
		data = 0
	}
	hs.m[s] = data + 1
}

/*
func (hs *captureState) get(s string) interface{} {
	if hs.m == nil {
		return nil
	}
	hs.lock.RLock()
	defer hs.lock.RUnlock()
	v, ok := hs.m[s]
	if !ok {
		v = nil
	}
	return v
}
*/

func (hs *captureState) set(s string, n interface{}) {
	if hs.m == nil {
		return
	}
	hs.lock.Lock()
	defer hs.lock.Unlock()
	hs.m[s] = n
}

type apiHandler struct {
	*captureState
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sc := http.StatusOK
	m := make(map[string]interface{})
	m["id"] = "mock"
	m["name"] = "mock"

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)
	dec := json.NewDecoder(r.Body)
	var msg filestream.FsTransmitData
	err := dec.Decode(&msg)
	if err != nil {
		fmt.Println("ERROR", err)
	}

	/*
		tm := time.Now()
		tmOld, ok := h.get("time").(time.Time)
		diff := time.Duration(0)
		if ok {
			diff = tm.Sub(tmOld)
		}
		fmt.Printf("GOT %v %v %v\n", tm, diff, msg)
		h.set("time", tm)
	*/

	f := msg.Files[filestream.HistoryFileName]
	total := f.Offset
	total += len(f.Content)
	h.set("total", total)
	h.inc("records")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(sc)
	err = json.NewEncoder(w).Encode(m)
	if err != nil {
		panic(fmt.Sprintf("ERROR: %v", err))
	}
}

type testServer struct {
	hserver  *httptest.Server
	settings *service.Settings
	logger   *observability.CoreLogger
	mux      *http.ServeMux
}

func NewTestServer() *testServer {
	settings := service.Settings{}
	logger := observability.NewNoOpLogger()

	mux := http.NewServeMux()
	hserver := httptest.NewServer(mux)
	ts := &testServer{hserver: hserver, logger: logger, settings: &settings, mux: mux}
	return ts
}

func (ts *testServer) close() {
	ts.hserver.Close()
}

type filestreamTest struct {
	fs      filestream.FileStream
	capture *captureState
	path    string
	mux     *http.ServeMux
	tserver *testServer
}

func NewFilestreamTest(
	tName string,
	tServer *testServer,
	params filestream.FileStreamParams,
	fn func(fs filestream.FileStream),
) *filestreamTest {
	m := make(map[string]interface{})
	capture := captureState{m: m}
	fstreamPath := "/test/" + tName
	tServer.mux.Handle(fstreamPath, apiHandler{&capture})

	fs := filestream.NewFileStream(params)
	fs.SetPath(fstreamPath)
	fs.Start()

	fsTest := filestreamTest{capture: &capture, path: fstreamPath, mux: tServer.mux, fs: fs, tserver: tServer}
	defer fsTest.finish()
	fn(fsTest.fs)
	return &fsTest
}

func (tst *filestreamTest) finish() {
	tst.fs.Close()
	tst.tserver.close()
}

func NewHistoryRecord() *service.Record {
	msg := &service.Record{
		RecordType: &service.Record_History{
			History: &service.HistoryRecord{
				Step: &service.HistoryStep{Num: 0},
				Item: []*service.HistoryItem{
					{Key: "_runtime", ValueJson: fmt.Sprintf("%f", 0.0)},
					{Key: "_step", ValueJson: fmt.Sprintf("%d", 0)},
				}}}}
	return msg
}

func TestSendHistory(t *testing.T) {
	num := 10
	delay := 5 * time.Millisecond

	tServer := NewTestServer()
	fsParams := filestream.FileStreamParams{
		Settings: tServer.settings,
		Logger:   tServer.logger,
		ApiClient: apitest.TestingClient(
			tServer.hserver.URL,
			api.ClientOptions{},
		),
	}

	tst := NewFilestreamTest(
		t.Name(),
		tServer,
		fsParams,
		func(fs filestream.FileStream) {
			msg := NewHistoryRecord()
			for i := 0; i < num; i++ {
				time.Sleep(delay)
				fs.StreamRecord(msg)
			}
		},
	)
	assert.Equal(t, num, tst.capture.m["total"].(int))
}

func TestHeartbeat(t *testing.T) {
	lastTransmitTime := time.Now()
	heartbeatInterval := 1 * time.Millisecond

	tServer := NewTestServer()
	fsParams := filestream.FileStreamParams{
		Settings: tServer.settings,
		Logger:   tServer.logger,
		ApiClient: apitest.TestingClient(
			tServer.hserver.URL,
			api.ClientOptions{},
		),
		LastTransmitTime:  lastTransmitTime,
		HeartbeatInterval: heartbeatInterval,
	}

	tst := NewFilestreamTest(
		t.Name(),
		tServer,
		fsParams,
		func(fs filestream.FileStream) {
			time.Sleep(10 * time.Millisecond)
		},
	)
	assert.NotEqual(t, lastTransmitTime, tst.fs.GetLastTransmitTime())
}

func BenchmarkHistory(b *testing.B) {
	num := 10_000

	tServer := NewTestServer()
	fsParams := filestream.FileStreamParams{
		Settings: tServer.settings,
		Logger:   tServer.logger,
		ApiClient: apitest.TestingClient(
			tServer.hserver.URL,
			api.ClientOptions{},
		),
	}

	tst := NewFilestreamTest(
		b.Name(),
		tServer,
		fsParams,
		func(fs filestream.FileStream) {
			msg := NewHistoryRecord()
			b.ResetTimer()
			for i := 0; i < num; i++ {
				fs.StreamRecord(msg)
			}
		})
	assert.Equal(b, num, tst.capture.m["total"].(int))
}
