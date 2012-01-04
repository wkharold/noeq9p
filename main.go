package main

import (
	"code.google.com/p/go9p/p"
	"code.google.com/p/go9p/p/srv"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	workerIdBits       = uint64(5)
	datacenterIdBits   = uint64(5)
	maxWorkerId        = int64(-1) ^ (int64(-1) << workerIdBits)
	maxDatacenterId    = int64(-1) ^ (int64(-1) << datacenterIdBits)
	sequenceBits       = uint64(12)
	workerIdShift      = sequenceBits
	datacenterIdShift  = sequenceBits + workerIdBits
	timestampLeftShift = sequenceBits + workerIdBits + datacenterIdBits
	sequenceMask       = int64(-1) ^ (int64(-1) << sequenceBits)

	// Tue, 21 Mar 2006 20:50:14.000 GMT
	twepoch = int64(1288834974657)

	idsz = 17
)

// Flags
var (
	wid   = flag.Int64("w", 0, "worker id")
	did   = flag.Int64("d", 0, "datacenter id")
	laddr = flag.String("l", "0.0.0.0:4444", "the address to listen on")
	lts   = flag.Int64("t", -1, "the last timestamp in milliseconds")
)

var (
	mu   sync.Mutex
	sm   sync.Mutex
	root *srv.File
	rqs  uint64
	ids  uint64
	seq  int64
	fs   *srv.Fsrv
)

type CtlFile struct {
	srv.File
	datafile []byte
}

func (c *CtlFile) Read(fid *srv.FFid, buf []byte, offset uint64) (int, error) {
	c.Lock()
	defer c.Unlock()

	var b []byte

	n := len(c.datafile)
	if offset > uint64(0) {
		return 0, nil
	}

	b = c.datafile
	if len(buf) < n {
		n = len(buf)
	}

	copy(buf, b)
	return n, nil
}

func (c *CtlFile) Write(fid *srv.FFid, data []byte, offset uint64) (int, error) {
	c.Lock()
	defer c.Unlock()

	n, err := strconv.Atoi(string(data))
	if err != nil {
		log.Fatalf("%s\n", err)
		return -1, err
	}

	if n <= 0 {
		return len(data), nil
	}

	now := time.Now()

	sm.Lock()
	dfname := fmt.Sprintf("%d%d%d%d%d%d.%d", now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), rqs)
	sm.Unlock()

	df := new(DataFile)
	err = df.Add(c.File.Parent, dfname, p.OsUsers.Uid2User(os.Geteuid()), nil, 0777, df)
	if err != nil {
		return -1, &p.Error{"cannot create data file", 0}
	}

	df.uuids = make([][]byte, n)
	for i := uint(0); i < uint(n); i++ {
		id, err := nextId()
		if err != nil {
			return -1, err
		}

		uuid := fmt.Sprintf("%02x%02x%02x%02x%02x%02x%02x%02x\n",
			byte(id>>56),
			byte(id>>48),
			byte(id>>40),
			byte(id>>32),
			byte(id>>24),
			byte(id>>16),
			byte(id>>8),
			byte(id))

		df.uuids[i] = make([]byte, idsz)
		copy(df.uuids[i][:idsz], []byte(uuid)[:idsz])
	}
	df.Length = uint64(n * idsz)

	c.datafile = make([]byte, len(dfname))
	copy(c.datafile, []byte(dfname))

	sm.Lock()
	rqs++
	ids += uint64(n)
	sm.Unlock()

	return len(data), nil
}

func (c *CtlFile) Wstat(fid *srv.FFid, dir *p.Dir) error {
	var uid, gid uint32

	c.Lock()
	defer c.Unlock()

	up := fs.Upool
	uid = dir.Uidnum
	gid = dir.Gidnum
	if uid == p.NOUID && dir.Uid != "" {
		user := up.Uname2User(dir.Uid)
		if user == nil {
			return srv.Enouser
		}

		c.Uidnum = uint32(user.Id())
	}

	if gid == p.NOUID && dir.Gid != "" {
		group := up.Gname2Group(dir.Gid)
		if group == nil {
			return srv.Enouser
		}

		c.Gidnum = uint32(group.Id())
	}

	if dir.Mode != 0xFFFFFFFF {
		c.Mode = (c.Mode &^ 0777) | (dir.Mode & 0777)
	}

	if dir.Name != "" {
		if err := c.Rename(dir.Name); err != nil {
			return err
		}
	}

	return nil
}

type DataFile struct {
	srv.File
	uuids [][]byte
}

func (d *DataFile) Read(fid *srv.FFid, buf []byte, offset uint64) (int, error) {
	d.Lock()
	defer d.Unlock()

	if offset > d.Length {
		return 0, nil
	}

	count := len(buf)
	if offset+uint64(count) > d.Length {
		count = int(d.Length - offset)
	}

	for n, off, b := offset/uint64(idsz), offset%uint64(idsz), buf[0:count]; len(b) > 0; n++ {
		m := idsz - int(off)
		if m > len(b) {
			m = len(b)
		}

		blk := make([]byte, idsz)
		if len(d.uuids[n]) != 0 {
			blk = d.uuids[n]
		}

		copy(b, blk[off:off+uint64(m)])
		b = b[m:]
		off = 0
	}

	return count, nil
}

type Clone struct {
	srv.File
	clones int
}

func (k *Clone) Read(fid *srv.FFid, buf []byte, offset uint64) (int, error) {
	k.Lock()
	defer k.Unlock()

	var err error

	// we only allow a single read from us, change the offset and we're done
	if offset > uint64(0) {
		return 0, nil
	}

	user := p.OsUsers.Uid2User(os.Geteuid())

	k.clones++
	name := strconv.Itoa(k.clones)
	f := new(srv.File)
	err = f.Add(root, name, user, nil, p.DMDIR|0777, f)
	if err != nil {
		return 0, &p.Error{"can not create dir", 0}
	}

	ctl := new(CtlFile)
	ctl.datafile = make([]byte, 0)
	err = ctl.Add(f, "ctl", user, nil, 0777, ctl)
	if err != nil {
		return 0, &p.Error{"cannot create ctl file", 0}
	}

	b := []byte(name)
	if len(buf) < len(b) {
		f.Remove()
		return 0, &p.Error{"not enough buffer space for result", 0}
	}

	copy(buf, b)
	return len(b), nil
}

type StatsFile struct {
	srv.File
	data []byte
}

func (s *StatsFile) Read(fid *srv.FFid, buf []byte, offset uint64) (int, error) {
	s.Lock()
	defer s.Unlock()

	var b []byte

	sm.Lock()
	str := fmt.Sprintf("%d,%d\n", rqs, ids)
	sm.Unlock()

	b = []byte(str)

	n := len(b)
	if offset >= uint64(n) {
		return 0, nil
	}

	b = b[int(offset):n]
	n -= int(offset)
	if len(buf) < n {
		n = len(buf)
	}

	copy(buf[offset:int(offset)+n], b[offset:])
	return n, nil
}

func parseFlags() {
	flag.Parse()
	if *wid < 0 || *wid > maxWorkerId {
		log.Fatalf("worker id must be between 0 and %d", maxWorkerId)
	}

	if *did < 0 || *did > maxDatacenterId {
		log.Fatalf("datacenter id must be between 0 and %d", maxDatacenterId)
	}
}

func mkroot() (*srv.File, error) {
	root := new(srv.File)
	err := root.Add(nil, "/", p.OsUsers.Uid2User(os.Geteuid()), p.OsUsers.Gid2Group(os.Getegid()), p.DMDIR|0555, nil)
	if err != nil {
		return nil, err
	}
	return root, nil
}

func serve9p() {
	var cl *Clone
	var err error
	var sf *StatsFile

	root, err = mkroot()
	if err != nil {
		goto error
	}

	cl = new(Clone)
	err = cl.Add(root, "clone", p.OsUsers.Uid2User(os.Geteuid()), nil, 0444, cl)
	if err != nil {
		goto error
	}

	sf = new(StatsFile)
	err = sf.Add(root, "stats", p.OsUsers.Uid2User(os.Geteuid()), nil, 0444, sf)
	if err != nil {
		goto error
	}

	fs = srv.NewFileSrv(root)
	fs.Dotu = true

	fs.Start(fs)
	err = fs.StartNetListener("tcp", *laddr)
	if err != nil {
		goto error
	}
	return

error:
	log.Fatalf("Error serving 9p: %s\n", err)
}

func milliseconds() int64 {
	return time.Now().UnixNano() / 1e6
}

func nextId() (int64, error) {
	mu.Lock()
	defer mu.Unlock()

	ts := milliseconds()
	if *lts == ts {
		seq = (seq + 1) & sequenceMask
		if seq == 0 {
			for ts <= *lts {
				ts = milliseconds()
			}
		}
	} else {
		seq = 0
	}

	if ts < *lts {
		return 0, fmt.Errorf("time is moving backwards, waiting until %d\n", *lts)
	}

	*lts = ts

	id := ((ts - twepoch) << timestampLeftShift) |
		(*did << datacenterIdShift) |
		(*wid << workerIdShift) |
		seq

	return id, nil
}

func main() {
	parseFlags()
	serve9p()
}
