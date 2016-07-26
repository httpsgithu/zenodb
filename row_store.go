package tdb

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/getlantern/bytemap"
	"github.com/golang/snappy"
)

// TODO: add WAL

type rowStoreOptions struct {
	dir              string
	maxMemStoreBytes int
	maxFlushLatency  time.Duration
}

type flushRequest struct {
	idx  int
	ms   memStore
	sort bool
}

type rowStore struct {
	t             *table
	opts          *rowStoreOptions
	memStores     map[int]memStore
	fileStore     *fileStore
	inserts       chan *insert
	flushes       chan *flushRequest
	flushFinished chan time.Duration
	mx            sync.RWMutex
}

func (t *table) openRowStore(opts *rowStoreOptions) (*rowStore, error) {
	err := os.MkdirAll(opts.dir, 0755)
	if err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("Unable to create folder for row store: %v", err)
	}

	existingFileName := ""
	files, err := ioutil.ReadDir(opts.dir)
	if err != nil {
		return nil, fmt.Errorf("Unable to read contents of directory: %v", err)
	}
	if len(files) > 0 {
		existingFileName = filepath.Join(opts.dir, files[len(files)-1].Name())
		log.Debugf("Initializing row store from %v", existingFileName)
	}

	rs := &rowStore{
		opts:          opts,
		t:             t,
		memStores:     make(map[int]memStore, 2),
		inserts:       make(chan *insert),
		flushes:       make(chan *flushRequest, 1),
		flushFinished: make(chan time.Duration, 1),
		fileStore: &fileStore{
			t:        t,
			opts:     opts,
			filename: existingFileName,
		},
	}

	go rs.processInserts()
	go rs.processFlushes()

	return rs, nil
}

func (rs *rowStore) insert(insert *insert) {
	rs.inserts <- insert
}

func (rs *rowStore) processInserts() {
	memStoreIdx := 0
	memStoreBytes := 0
	currentMemStore := make(memStore)
	rs.memStores[memStoreIdx] = currentMemStore

	flushInterval := rs.opts.maxFlushLatency
	flushIdx := 0
	flush := func() {
		if memStoreBytes == 0 {
			// nothing to flush
			return
		}
		log.Debugf("Requesting flush at memstore size: %v", humanize.Bytes(uint64(memStoreBytes)))
		memStoreCopy := currentMemStore.copy()
		shouldSort := flushIdx%10 == 0
		shouldSort = false
		fr := &flushRequest{memStoreIdx, memStoreCopy, shouldSort}
		rs.mx.Lock()
		flushIdx++
		currentMemStore = make(memStore, len(currentMemStore))
		memStoreIdx++
		rs.memStores[memStoreIdx] = currentMemStore
		memStoreBytes = 0
		rs.mx.Unlock()
		rs.flushes <- fr
	}

	flushTimer := time.NewTimer(flushInterval)

	for {
		select {
		case insert := <-rs.inserts:
			truncateBefore := rs.t.truncateBefore()
			seqs := currentMemStore[insert.key]
			if seqs == nil {
				memStoreBytes += len(insert.key)
			}
			rs.mx.Lock()
			// Grow sequences to match number of fields in table
			for i := len(seqs); i < len(rs.t.Fields); i++ {
				seqs = append(seqs, nil)
			}
			for i, field := range rs.t.Fields {
				current := seqs[i]
				previousSize := len(current)
				updated := current.update(insert.vals, field, rs.t.Resolution, truncateBefore)
				seqs[i] = updated
				memStoreBytes += len(updated) - previousSize
			}
			currentMemStore[insert.key] = seqs
			rs.mx.Unlock()
			if memStoreBytes >= rs.opts.maxMemStoreBytes {
				flush()
			}
		case <-flushTimer.C:
			flush()
		case flushDuration := <-rs.flushFinished:
			flushTimer.Reset(flushDuration * 10)
		}
	}
}

func (rs *rowStore) iterate(onValue func(bytemap.ByteMap, []sequence)) error {
	rs.mx.RLock()
	fs := rs.fileStore
	memStoresCopy := make([]memStore, 0, len(rs.memStores))
	for _, ms := range rs.memStores {
		memStoresCopy = append(memStoresCopy, ms.copy())
	}
	rs.mx.RUnlock()
	return fs.iterate(onValue, memStoresCopy...)
}

func (rs *rowStore) processFlushes() {
	for req := range rs.flushes {
		start := time.Now()
		out, err := ioutil.TempFile("", "nextrowstore")
		if err != nil {
			panic(err)
		}
		sout := snappy.NewWriter(out)
		cout := bufio.NewWriterSize(sout, 65536)

		// if req.sort {
		// 	sd := &sortData{rs, req.ms, cout}
		// 	err = emsort.Sorted(sd, rs.opts.maxMemStoreBytes/2)
		// 	if err != nil {
		// 		panic(fmt.Errorf("Unable to process flush: %v", err))
		// 	}
		// } else {
		// TODO: DRY violation with sortData.Fill sortData.OnSorted
		truncateBefore := rs.t.truncateBefore()
		write := func(key bytemap.ByteMap, columns []sequence) {
			hasActiveSequence := false
			for i, seq := range columns {
				seq = seq.truncate(rs.t.Fields[i].EncodedWidth(), rs.t.Resolution, truncateBefore)
				columns[i] = seq
				if seq != nil {
					hasActiveSequence = true
				}
			}

			if !hasActiveSequence {
				// all sequences expired, remove key
				return
			}

			// keylength|key|numcolumns|col1len|col2len|...|lastcollen|col1|col2|...|lastcol
			err = binary.Write(cout, binaryEncoding, uint16(len(key)))
			if err != nil {
				panic(err)
			}
			_, err = cout.Write(key)
			if err != nil {
				panic(err)
			}

			err = binary.Write(cout, binaryEncoding, uint16(len(columns)))
			if err != nil {
				panic(err)
			}
			for _, seq := range columns {
				err = binary.Write(cout, binaryEncoding, uint64(len(seq)))
				if err != nil {
					panic(err)
				}
			}

			for _, seq := range columns {
				_, err = cout.Write(seq)
				if err != nil {
					panic(err)
				}
			}
		}
		rs.mx.RLock()
		fs := rs.fileStore
		rs.mx.RUnlock()
		fs.iterate(write, req.ms)
		// }
		err = cout.Flush()
		if err != nil {
			panic(err)
		}
		err = sout.Close()
		if err != nil {
			panic(err)
		}

		fi, err := out.Stat()
		if err != nil {
			log.Errorf("Unable to stat output file to get size: %v", err)
		}
		// Note - we left-pad the unix nano value to the widest possible length to
		// ensure lexicographical sort matches time-based sort (e.g. on directory
		// listing).
		newFileStoreName := filepath.Join(rs.opts.dir, fmt.Sprintf("filestore_%020d.dat", time.Now().UnixNano()))
		err = os.Rename(out.Name(), newFileStoreName)
		if err != nil {
			panic(err)
		}

		oldFileStore := rs.fileStore.filename
		rs.mx.Lock()
		delete(rs.memStores, req.idx)
		rs.fileStore = &fileStore{rs.t, rs.opts, newFileStoreName}
		rs.mx.Unlock()

		// TODO: add background process for cleaning up old file stores
		if oldFileStore != "" {
			go func() {
				time.Sleep(5 * time.Minute)
				err := os.Remove(oldFileStore)
				if err != nil {
					log.Errorf("Unable to delete old file store, still consuming disk space unnecessarily: %v", err)
				}
			}()
		}

		flushDuration := time.Now().Sub(start)
		rs.flushFinished <- flushDuration
		wasSorted := "not sorted"
		if req.sort {
			wasSorted = "sorted"
		}
		if fi != nil {
			log.Debugf("Flushed to %v in %v, size %v. %v.", newFileStoreName, flushDuration, humanize.Bytes(uint64(fi.Size())), wasSorted)
		} else {
			log.Debugf("Flushed to %v in %v. %v.", newFileStoreName, flushDuration, wasSorted)
		}
	}
}

type memStore map[string][]sequence

func (ms memStore) remove(key string) []sequence {
	seqs, found := ms[key]
	if found {
		delete(ms, key)
	}
	return seqs
}

func (ms memStore) copy() memStore {
	memStoreCopy := make(map[string][]sequence, len(ms))
	for key, seqs := range ms {
		memStoreCopy[key] = seqs
	}
	return memStoreCopy
}

// fileStore stores rows on disk, encoding them as:
//   keylength|key|numcolumns|col1len|col2len|...|lastcollen|col1|col2|...|lastcol
//
// keylength is 16 bits
// key can be up to 64KB
// numcolumns is 16 bits (i.e. 65,536 columns allowed)
// col*end is 64 bits
type fileStore struct {
	t        *table
	opts     *rowStoreOptions
	filename string
}

func (fs *fileStore) iterate(onRow func(bytemap.ByteMap, []sequence), memStores ...memStore) error {
	if log.IsTraceEnabled() {
		log.Tracef("Iterating with %d memstores from file %v", len(memStores), fs.filename)
	}

	truncateBefore := fs.t.truncateBefore()
	file, err := os.OpenFile(fs.filename, os.O_RDONLY, 0)
	if !os.IsNotExist(err) {
		if err != nil {
			return fmt.Errorf("Unable to open file %v: %v", fs.filename, err)
		}
		r := snappy.NewReader(bufio.NewReaderSize(file, 65536))

		// Read from file
		for {
			// keylength|key|numcolumns|col1len|col2len|...|lastcollen|col1|col2|...|lastcol
			keyLength := uint16(0)
			err := binary.Read(r, binaryEncoding, &keyLength)
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("Unexpected error reading key length: %v", err)
			}

			key := make(bytemap.ByteMap, keyLength)
			_, err = io.ReadFull(r, key)
			if err != nil {
				return fmt.Errorf("Unexpected error reading key: %v", err)
			}

			numColumns := uint16(0)
			err = binary.Read(r, binaryEncoding, &numColumns)
			if err != nil {
				return fmt.Errorf("Unable to read numColumns: %v", err)
			}

			colLengths := make([]int, 0, numColumns)
			for i := uint16(0); i < numColumns; i++ {
				colLength := uint64(0)
				err = binary.Read(r, binaryEncoding, &colLength)
				if err != nil {
					return fmt.Errorf("Unable to read colLength: %v", err)
				}
				colLengths = append(colLengths, int(colLength))
			}

			columns := make([]sequence, 0, numColumns)
			for i, colLength := range colLengths {
				seq := make(sequence, colLength)
				_, err = io.ReadFull(r, seq)
				if err != nil {
					return fmt.Errorf("Unexpected error reading seq: %v", err)
				}
				columns = append(columns, seq)
				if log.IsTraceEnabled() {
					log.Tracef("File Read: %v", seq.String(fs.t.Fields[i]))
				}
			}

			for _, ms := range memStores {
				columns2 := ms.remove(string(key))
				for i := 0; i < len(columns) || i < len(columns2); i++ {
					if i >= len(columns2) {
						// nothing to merge
						continue
					}
					if i >= len(columns) {
						// nothing to merge, just add new column
						columns = append(columns, columns2[i])
						continue
					}
					columns[i] = columns[i].merge(columns2[i], fs.t.Fields[i], fs.t.Resolution, truncateBefore)
				}
			}

			onRow(key, columns)
		}
	}

	// Read remaining stuff from mem stores
	for i, ms := range memStores {
		for key, columns := range ms {
			for j := i + 1; j < len(memStores); j++ {
				ms2 := memStores[j]
				columns2 := ms2.remove(string(key))
				for i := 0; i < len(columns) || i < len(columns2); i++ {
					if i >= len(columns2) {
						// nothing to merge
						continue
					}
					if i >= len(columns) {
						// nothing to merge, just add new column
						columns = append(columns, columns2[i])
						continue
					}
					columns[i] = columns[i].merge(columns2[i], fs.t.Fields[i], fs.t.Resolution, truncateBefore)
				}
			}
			onRow(bytemap.ByteMap(key), columns)
		}
	}

	return nil
}

// type sortData struct {
// 	rs  *rowStore
// 	ms  memStore
// 	out io.Writer
// }
//
// func (sd *sortData) Fill(fn func([]byte) error) error {
// 	periodWidth := sd.rs.opts.ex.EncodedWidth()
// 	truncateBefore := sd.rs.opts.truncateBefore()
// 	doFill := func(key bytemap.ByteMap, seq sequence) {
// 		seq = seq.truncate(periodWidth, sd.rs.opts.resolution, truncateBefore)
// 		if seq == nil {
// 			// entire sequence is expired, remove it
// 			return
// 		}
// 		b := make([]byte, width16bits+width64bits+len(key)+len(seq))
// 		binaryEncoding.PutUint16(b, uint16(len(key)))
// 		binaryEncoding.PutUint64(b[width16bits:], uint64(len(seq)))
// 		copy(b[width16bits+width64bits:], key)
// 		copy(b[width16bits+width64bits+len(key):], seq)
// 		fn(b)
// 	}
// 	sd.rs.mx.RLock()
// 	fs := sd.rs.fileStore
// 	sd.rs.mx.RUnlock()
// 	fs.iterate(doFill, sd.ms)
// 	return nil
// }
//
// func (sd *sortData) Read(r io.Reader) ([]byte, error) {
// 	b := make([]byte, width16bits+width64bits)
// 	_, err := io.ReadFull(r, b)
// 	if err != nil {
// 		return nil, err
// 	}
// 	keyLength := binaryEncoding.Uint16(b)
// 	seqLength := binaryEncoding.Uint64(b[width16bits:])
// 	b2 := make([]byte, len(b)+int(keyLength)+int(seqLength))
// 	_b2 := b2
// 	copy(_b2, b)
// 	_b2 = _b2[width16bits+width64bits:]
// 	_, err = io.ReadFull(r, _b2)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return b2, nil
// }
//
// func (sd *sortData) Less(a []byte, b []byte) bool {
// 	// We compare key/value pairs by doing a lexicographical comparison on the
// 	// longest portion of the key that's available in both values.
// 	keyLength := binaryEncoding.Uint16(a)
// 	bKeyLength := binaryEncoding.Uint16(b)
// 	if bKeyLength < keyLength {
// 		keyLength = bKeyLength
// 	}
// 	s := width16bits + width64bits // exclude key and seq length header
// 	e := s + int(keyLength)
// 	return bytes.Compare(a[s:e], b[s:e]) < 0
// }
//
// func (sd *sortData) OnSorted(b []byte) error {
// 	_, err := sd.out.Write(b)
// 	return err
// }
