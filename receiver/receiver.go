package receiver

import (
	"bufio"
	"bytes"
	"fmt"
	"l2met/bucket"
	"l2met/conf"
	"l2met/store"
	"l2met/utils"
	"sync"
	"sync/atomic"
	"time"
)

// We read the body of an http request and then close the request.
// The processing of the body happens in a seperate routine. We use
// this struct to hold the data that is passed inbetween routines.
type LogRequest struct {
	// The body of the HTTP request.
	Body []byte
	// Options from the query parameters
	Opts map[string][]string
}

type register struct {
	sync.Mutex
	m map[bucket.Id]*bucket.Bucket
}

type Receiver struct {
	// Keeping a register allows us to aggregate buckets in memory.
	// This decouples redis writes from HTTP requests.
	Register *register
	// After we pull data from the HTTP requests,
	// We put the data in the inbox to be processed.
	Inbox chan *LogRequest
	// The interval at which things are moved fron the inbox to the outbox
	TransferTicker *time.Ticker
	// After we flush our register of buckets, we put the
	// buckets in this channel to be flushed to redis.
	Outbox chan *bucket.Bucket
	// Flush buckets from register to redis. Number of seconds.
	FlushInterval time.Duration
	// How many outlet routines should be running.
	NumOutlets int
	// How many accept routines should be running.
	NumAcceptors int
	// Bucket storage.
	Store store.Store
	//Count the number of times we accept a bucket.
	numBuckets uint64
}

func NewReceiver(sz, c int, i time.Duration, s store.Store) *Receiver {
	r := new(Receiver)
	r.Inbox = make(chan *LogRequest, sz)
	r.Outbox = make(chan *bucket.Bucket, sz)
	r.Register = &register{m: make(map[bucket.Id]*bucket.Bucket)}
	r.numBuckets = uint64(0)
	r.FlushInterval = i
	r.NumOutlets = c
	r.NumAcceptors = c
	r.Store = s
	return r
}

func (r *Receiver) Receive(b []byte, opts map[string][]string) {
	defer r.measure("receive", time.Now())
	r.Inbox <- &LogRequest{b, opts}
}

func (r *Receiver) Start() {
	// Parsing the log data can be expensive. Make use
	// of parallelism.
	for i := 0; i < r.NumAcceptors; i++ {
		go r.Accept()
	}
	// Each outlet will write a bucket to redis.
	for i := 0; i < r.NumOutlets; i++ {
		go r.Outlet()
	}
	r.TransferTicker = time.NewTicker(r.FlushInterval)
	// The transfer is not a concurrent process.
	// It removes buckets from the register to the outbox.
	go r.Transfer()
}

func (r *Receiver) Stop() {
	r.TransferTicker.Stop()
	// We sleep to give our transfer routine time to finish.
	time.Sleep(r.FlushInterval)
	close(r.Inbox)
	close(r.Outbox)
}

func (r *Receiver) Accept() {
	for lreq := range r.Inbox {
		rdr := bufio.NewReader(bytes.NewReader(lreq.Body))
		for bucket := range bucket.NewBuckets(rdr, lreq.Opts) {
			now := time.Now().Truncate(bucket.Id.Resolution)
			if bucket.Id.Time.Equal(now) {
				r.addRegister(bucket)
			} else {
				fmt.Printf("at=receiver-drop b=%s n=%s\n",
					bucket.Id.Time, now)
				r.measure("drop", now)
			}
		}
	}
}

func (r *Receiver) addRegister(b *bucket.Bucket) {
	r.Register.Lock()
	defer r.Register.Unlock()
	k := *b.Id
	_, present := r.Register.m[k]
	if !present {
		r.Register.m[k] = b
	} else {
		r.Register.m[k].Add(b)
	}
	r.numBuckets += 1
}

func (r *Receiver) Transfer() {
	for _ = range r.TransferTicker.C {
		for k := range r.Register.m {
			r.Register.Lock()
			if m, ok := r.Register.m[k]; ok {
				delete(r.Register.m, k)
				r.Register.Unlock()
				r.Outbox <- m
			} else {
				r.Register.Unlock()
			}
		}
	}
}

func (r *Receiver) Outlet() {
	for b := range r.Outbox {
		if err := r.Store.Put(b); err != nil {
			fmt.Printf("error=%s\n", err)
		}
	}
}

func (r *Receiver) measure(name string, t time.Time) {
	if !conf.OutletMeasurements {
		return
	}
	b := &bucket.Bucket{
		Id: &bucket.Id{
			Name:       conf.AppName + ".receiver." + name,
			Source:     "",
			User:       conf.OutletUser,
			Pass:       conf.OutletPass,
			Time:       t.Truncate(time.Minute),
			Resolution: time.Minute,
		},
		Vals: []float64{float64(time.Since(t) / time.Millisecond)},
	}
	r.addRegister(b)
}

// Keep an eye on the lenghts of our bufferes. If they are maxed out, something
// is going wrong.
func (r *Receiver) Report() {
	for _ = range time.Tick(time.Second * 2) {
		na := atomic.LoadUint64(&r.numBuckets)
		atomic.AddUint64(&r.numBuckets, -na)
		utils.MeasureI("receiver.buckets", "buckets", int64(na))
		utils.MeasureI("receiver.inbox", "requests", int64(len(r.Inbox)))
		utils.MeasureI("receiver.register", "buckets", int64(len(r.Register.m)))
		utils.MeasureI("receiver.outbox", "buckets", int64(len(r.Outbox)))
	}
}
