package main

import (
	"errors"
	"fmt"
	"github.com/Dieterbe/go-metrics"
	"io"
	"net"
	"os"
	"time"
)

var bufio_buffer_size = 2000000 // in bytes. 4096 is go default

// to make sure writes to In are fast until we really can't keep up
var conn_in_buffer = 30000 // in metrics. (each metric line is typically about 70 bytes)

var keepsafe_initial_cap = 100000 // not very important

// this interval should be long enough to capture all failure modes
// (endpoint down, delayed timeout, etc), so it should be at least as long as the flush interval
var keepsafe_keep_duration = time.Duration(10 * time.Second)

var newLine = []byte{'\n'}

type Conn struct {
	conn        *net.TCPConn
	buffered    *Writer
	shutdown    chan bool
	In          chan []byte
	dest        *Destination // which dest do we correspond to
	up          bool
	pickle      bool
	checkUp     chan bool
	updateUp    chan bool
	flush       chan bool
	flushErr    chan error
	periodFlush time.Duration
	unFlushed   []byte
	keepSafe    *keepSafe

	numErrTruncated   metrics.Counter
	numErrWrite       metrics.Counter
	numErrFlush       metrics.Counter
	numOut            metrics.Counter
	durationWrite     metrics.Timer
	durationTickFlush metrics.Timer
	durationManuFlush metrics.Timer
	tickFlushSize     metrics.Histogram
	manuFlushSize     metrics.Histogram
	numBuffered       metrics.Gauge
	numDropBadPickle  metrics.Counter
}

func NewConn(addr string, dest *Destination, periodFlush time.Duration, pickle bool) (*Conn, error) {
	raddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	laddr, _ := net.ResolveTCPAddr("tcp", "0.0.0.0")
	conn, err := net.DialTCP("tcp", laddr, raddr)
	if err != nil {
		return nil, err
	}
	cleanAddr := addrToPath(addr)
	connObj := &Conn{
		conn:              conn,
		buffered:          NewWriter(conn, bufio_buffer_size, cleanAddr),
		shutdown:          make(chan bool, 1), // when we write here, HandleData() may not be running anymore to read from the chan
		In:                make(chan []byte, conn_in_buffer),
		dest:              dest,
		up:                true,
		pickle:            pickle,
		checkUp:           make(chan bool),
		updateUp:          make(chan bool),
		flush:             make(chan bool),
		flushErr:          make(chan error),
		periodFlush:       periodFlush,
		keepSafe:          NewKeepSafe(keepsafe_initial_cap, keepsafe_keep_duration),
		numErrTruncated:   Counter("dest=" + cleanAddr + ".unit=Err.type=truncated"),
		numErrWrite:       Counter("dest=" + cleanAddr + ".unit=Err.type=write"),
		numErrFlush:       Counter("dest=" + cleanAddr + ".unit=Err.type=flush"),
		numOut:            Counter("dest=" + cleanAddr + ".unit=Metric.direction=out"),
		durationWrite:     Timer("dest=" + cleanAddr + ".what=durationWrite"),
		durationTickFlush: Timer("dest=" + cleanAddr + ".what=durationFlush.type=ticker"),
		durationManuFlush: Timer("dest=" + cleanAddr + ".what=durationFlush.type=manual"),
		tickFlushSize:     Histogram("dest=" + cleanAddr + ".unit=B.what=FlushSize.type=ticker"),
		manuFlushSize:     Histogram("dest=" + cleanAddr + ".unit=B.what=FlushSize.type=manual"),
		numBuffered:       Gauge("dest=" + cleanAddr + ".unit=Metric.what=numBuffered"),
		numDropBadPickle:  Counter("dest=" + cleanAddr + ".unit=Metric.action=drop.reason=bad_pickle"),
	}

	go connObj.checkEOF()

	go connObj.HandleData()
	go connObj.HandleStatus()
	return connObj, nil
}

func (c *Conn) isAlive() bool {
	return <-c.checkUp
}

// normally the remote end should never write anything back
// but we know when we get EOF that the other end closed the conn
// if not for this, we can happily write and flush without getting errors (in Go) but getting RST tcp packets back (!)
// props to Tv` for this trick.
func (c *Conn) checkEOF() {
	b := make([]byte, 1024)
	for {
		num, err := c.conn.Read(b)
		if err == io.EOF {
			log.Notice("conn %s .conn.Read returned EOF -> conn is closed. closing conn explicitly", c.dest.Addr)
			c.Close()
			return
		}
		// just in case i misunderstand something or the remote behaves badly
		if num != 0 {
			log.Debug("conn %s .conn.Read data? did not expect that.  data: %s\n", c.dest.Addr, b[:num])
		}
		if err != io.EOF {
			log.Error("conn %s checkEOF .conn.Read returned err != EOF, which is unexpected.  closing conn. error: %s\n", c.dest.Addr, err)
			c.Close()
			return
		}
	}
}

// all these messages should potentially be resubmitted, because we're not confident about their delivery
// note: getting this data means resetting it! so handle it wisely.
// we also read out the In channel until it blocks.  Don't send any more input after calling this.
func (c *Conn) getRedo() [][]byte {
	// drain In queue in case we still had some data buffered.
	// normally this channel should already have been closed by the time we call this, but this is hard/complicated to enforce
	// so instead let's leverage a select. as soon as it blocks (due to chan close or no more input but not closed yet) we know we're
	// done reading and move on. it's easy to prove in the implementer that we don't send any more data to In after calling this
	for {
		select {
		case buf := <-c.In:
			c.numBuffered.Dec(1)
			c.keepSafe.Add(buf)
		default:
			return c.keepSafe.GetAll()
		}
	}
}

func (c *Conn) HandleStatus() {
	for {
		select {
		// note: when we mark as down here, it is expected that conn doesn't absorb any more data,
		// so that you can call getRedo() and get the full picture
		// this is actually not true yet.
		case c.up = <-c.updateUp:
			log.Debug("conn %s .up set to %v\n", c.dest.Addr, c.up)
		case c.checkUp <- c.up:
			log.Debug("conn %s .up query responded with %t", c.dest.Addr, c.up)
		}
	}
}

func (c *Conn) HandleData() {
	periodFlush := c.periodFlush
	tickerFlush := time.NewTicker(periodFlush)
	var now time.Time
	var durationActive time.Duration
	flushSize := int64(0)

	for {
		start := time.Now()
		var active time.Time
		var action string
		select { // handle incoming data or flush/shutdown commands
		// note that Writer.Write() can potentially cause a flush and hence block
		// choose the size of In based on how long these loop iterations take
		case buf := <-c.In:
			// seems to take about 30 micros when writing log to disk, 10 micros otherwise (100k messages/second)
			active = time.Now()
			c.numBuffered.Dec(1)
			action = "write"
			log.Info("conn %s HandleData: writing %s\n", c.dest.Addr, string(buf))
			c.keepSafe.Add(buf)
			n, err := c.Write(buf)
			if err != nil {
				log.Warning("conn %s write error: %s\n", c.dest.Addr, err)
				log.Debug("conn %s setting up=false\n", c.dest.Addr)
				c.updateUp <- false // assure In won't receive more data because every loop that writes to In reads this out
				log.Debug("conn %s Closing\n", c.dest.Addr)
				go c.Close() // this can take a while but that's ok. this conn won't be used anymore
				return
			}
			c.numOut.Inc(1)
			flushSize += int64(n)
			now = time.Now()
			durationActive = now.Sub(active)
			c.durationWrite.Update(durationActive)
		case <-tickerFlush.C:
			active = time.Now()
			action = "auto-flush"
			log.Debug("conn %s HandleData: c.buffered auto-flushing...\n", c.dest.Addr)
			err := c.buffered.Flush()
			if err != nil {
				log.Warning("conn %s HandleData c.buffered auto-flush done but with error: %s, closing\n", c.dest.Addr, err)
				c.numErrFlush.Inc(1)
				c.updateUp <- false
				go c.Close()
				return
			}
			log.Debug("conn %s HandleData c.buffered auto-flush done without error\n", c.dest.Addr)
			now = time.Now()
			durationActive = now.Sub(active)
			c.durationTickFlush.Update(durationActive)
			c.tickFlushSize.Update(flushSize)
			flushSize = 0
		case <-c.flush:
			active = time.Now()
			action = "manual-flush"
			log.Debug("conn %s HandleData: c.buffered manual flushing...\n", c.dest.Addr)
			err := c.buffered.Flush()
			c.flushErr <- err
			if err != nil {
				log.Warning("conn %s HandleData c.buffered manual flush done but witth error: %s, closing\n", c.dest.Addr, err)
				// TODO instrument
				c.updateUp <- false
				go c.Close()
				return
			}
			log.Notice("conn %s HandleData c.buffered manual flush done without error\n", c.dest.Addr)
			now = time.Now()
			durationActive = now.Sub(active)
			c.durationManuFlush.Update(durationActive)
			c.manuFlushSize.Update(flushSize)
			flushSize = 0
		case <-c.shutdown:
			active = time.Now()
			log.Debug("conn %s HandleData: shutdown received. returning.\n", c.dest.Addr)
			return
		}
		log.Debug("conn %s HandleData %s %s (total iter %s) (use this to tune your In buffering)\n", c.dest.Addr, action, durationActive, now.Sub(start))
	}
}

// returns a network/write error, so that it can be retried later
// deals with pickle errors internally because retrying wouldn't help anyway
func (c *Conn) Write(buf []byte) (int, error) {
	if c.pickle {
		dp, err := parseDataPoint(buf)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			c.numDropBadPickle.Inc(1)
			return 0, nil
		}
		buf = pickle(dp)
	}
	written := 0
	size := len(buf)
	n, err := c.buffered.Write(buf)
	written += n
	if err == nil && size == n && !c.pickle {
		size = 1
		n, err = c.buffered.Write(newLine)
		written += n
	}
	if err != nil {
		c.numErrWrite.Inc(1)
	}
	if err == nil && size != n {
		c.numErrTruncated.Inc(1)
		err = errors.New(fmt.Sprintf("truncated write: %s", string(buf)))
	}
	return written, err
}

func (c *Conn) Flush() error {
	log.Debug("conn %s going to flush my buffer\n", c.dest.Addr)
	c.flush <- true
	log.Debug("conn %s waiting for flush, getting error.\n", c.dest.Addr)
	return <-c.flushErr
}

func (c *Conn) Close() error {
	c.updateUp <- false // redundant in case HandleData() called us, but not if the dest called us
	log.Debug("conn %s Close() called. sending shutdown\n", c.dest.Addr)
	c.shutdown <- true
	log.Debug("conn %s c.conn.Close()\n", c.dest.Addr)
	a := c.conn.Close()
	log.Debug("conn %s c.conn is closed\n", c.dest.Addr)
	return a
}
