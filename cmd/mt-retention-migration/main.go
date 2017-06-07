package main

import (
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/raintank/metrictank/cluster"
	"github.com/raintank/metrictank/conf"
	"github.com/raintank/metrictank/idx/cassandra"
	"github.com/raintank/metrictank/mdata"
	"github.com/raintank/metrictank/mdata/cache"
	"github.com/raintank/metrictank/mdata/chunk"
	"gopkg.in/raintank/schema.v1"
)

var (
	bufferSize   = 1000
	printLock    = sync.Mutex{}
	source_table = "metric_1024"
)
var day_sec int64 = 60 * 60 * 24

type migrater struct {
	casIdx          *cassandra.CasIdx
	session         *gocql.Session
	chunkChan       chan *chunkDay
	metricCount     int
	readChunkCount  int
	writeChunkCount int
	ttlTables       mdata.TTLTables
	ttls            []uint32
}

type chunkDay struct {
	tableName string
	id        string
	ttl       uint32
	itergens  []chunk.IterGen
}

func main() {
	cassFlags := cassandra.ConfigSetup()
	fmt.Println(fmt.Sprintf("%+v", os.Args[1:]))
	cassFlags.Parse(os.Args[1:])
	cassFlags.Usage = cassFlags.PrintDefaults
	cassandra.Enabled = true
	cluster.Init("migrator", "0", time.Now(), "", -1)
	cluster.Manager.SetPrimary(true)
	casIdx := cassandra.New()
	err := casIdx.InitBare()
	if err != nil {
		throwError(err.Error())
	}

	ttls := make([]uint32, 3)
	ttls[0] = 60 * 60 * 24
	ttls[1] = 60 * 60 * 24 * 60
	ttls[2] = 60 * 60 * 24 * 365 * 3

	ttlTables := mdata.GetTTLTables(ttls, 20, mdata.Table_name_format)

	m := &migrater{
		casIdx:    casIdx,
		session:   casIdx.Session,
		chunkChan: make(chan *chunkDay, bufferSize),
		ttlTables: ttlTables,
		ttls:      ttls,
	}

	m.Start()
}

func (m *migrater) Start() {
	go m.read()
	m.write()
	printLock.Lock()
	fmt.Println(
		fmt.Sprintf(
			"Finished. Metrics: %d, Read chunks: %d, Wrote chunks: %d",
			m.metricCount,
			m.readChunkCount,
			m.writeChunkCount,
		),
	)
	printLock.Unlock()
}

func (m *migrater) read() {
	defs := m.casIdx.Load(nil)
	fmt.Println(fmt.Sprintf("received %d metrics", len(defs)))

	for _, metric := range defs {
		m.processMetric(&metric)
		m.metricCount++
	}

	close(m.chunkChan)
}

func (m *migrater) processMetric(def *schema.MetricDefinition) {
	now := time.Now().Unix()
	start := (now - (68 * day_sec))
	start_month := start / mdata.Month_sec
	end_month := (now - 1) / mdata.Month_sec

	for month := start_month; month <= end_month; month++ {
		row_key := fmt.Sprintf("%s_%d", def.Id, month)
		fmt.Println(fmt.Sprintf("select for row_key %s", row_key))
		itgenCount := 0
		for from := start_month * mdata.Month_sec; from <= (month+1)*mdata.Month_sec; from += day_sec {
			to := from + day_sec
			query := fmt.Sprintf(
				"SELECT ts, data FROM %s WHERE key = ? AND ts > ? AND ts <= ? ORDER BY ts ASC",
				source_table,
			)
			it := m.session.Query(query, row_key, from, to).Iter()
			itgens := m.process(it)
			itgenCount += len(itgens)
			m.generateChunks(itgens, def)
		}
		fmt.Println(fmt.Sprintf("%d chunks for row_key %s", itgenCount, row_key))
	}
}

func (m *migrater) process(it *gocql.Iter) []chunk.IterGen {
	var b []byte
	var ts int
	var itgens []chunk.IterGen

	for it.Scan(&ts, &b) {
		itgen, err := chunk.NewGen(b, uint32(ts))
		if err != nil {
			throwError(fmt.Sprintf("Error generating Itgen: %q", err))
		}

		itgens = append(itgens, *itgen)
	}
	err := it.Close()
	if err != nil {
		throwError(fmt.Sprintf("cassandra query error. %s", err))
	}

	return itgens
}

func (m *migrater) generateChunks(itgens []chunk.IterGen, def *schema.MetricDefinition) {
	cd := chunkDay{
		itergens: itgens,
	}
	m.readChunkCount += len(itgens)

	// if interval is larger than 30min we can directly write the chunk to
	// the highest retention table
	if def.Interval > 60*30 {
		cd.ttl = m.ttls[2]
		cd.tableName = m.ttlTables[m.ttls[2]].Table
		cd.id = def.Id

		m.chunkChan <- &cd
		return
	}

	now := uint32(time.Now().Unix())
	// chunks older than 60 days can be dropped
	dropBefore := now - 60*60*24*60
	// don't need raw older than 1 day
	noRawBefore := now - 60*60*24

	// if interval <1min, then create one min rollups
	if def.Interval < 60 {
		outChunkSpan := uint32(6 * 60 * 60)

		am := mdata.NewAggMetric(
			mdata.NewMockStore(),
			&cache.MockCache{},
			def.Id,
			[]conf.Retention{
				conf.NewRetentionMT(60, m.ttls[1], outChunkSpan, 2, true),
			},
			&conf.Aggregation{
				"default",
				regexp.MustCompile(".*"),
				0.5,
				// sum should not be necessary because that comes with Avg
				[]conf.Method{conf.Avg, conf.Lst, conf.Max, conf.Min},
			},
			false,
		)

		for _, itgen := range itgens {
			if itgen.Ts < dropBefore {
				continue
			}
			iter, err := itgen.Get()
			if err != nil {
				throwError(
					fmt.Sprintf("Corrupt chunk %s at ts %d", def.Id, itgen.Ts),
				)
			}
			for iter.Next() {
				ts, val := iter.Values()
				am.Add(ts, val)
			}
		}

		for _, agg := range am.GetAggregators() {
			for _, aggMetric := range agg.GetAggMetrics() {
				itgensNew := make([]chunk.IterGen, len(am.Chunks))
				for _, c := range aggMetric.Chunks {
					if !c.Closed {
						c.Finish()
					}
					itgensNew = append(itgensNew, *chunk.NewBareIterGen(
						c.Bytes(),
						c.T0,
						c.LastTs-c.T0,
					))
				}

				rollupChunkDay := chunkDay{
					ttl:       m.ttls[1],
					tableName: m.ttlTables[m.ttls[1]].Table,
					id:        aggMetric.Key,
					itergens:  itgensNew,
				}
				m.chunkChan <- &rollupChunkDay
			}
		}
	}

	for _, itgen := range cd.itergens {
		if itgen.Ts+itgen.Span < noRawBefore {
			continue
		}
	}
	m.chunkChan <- &cd
}

func (m *migrater) write() {
	for {
		chunk, more := <-m.chunkChan
		if !more {
			return
		}

		m.insertChunks(chunk.tableName, chunk.id, chunk.ttl, chunk.itergens)
		m.writeChunkCount++
	}
}

func (m *migrater) insertChunks(table, id string, ttl uint32, itergens []chunk.IterGen) {
	query := fmt.Sprintf("INSERT INTO %s (key, ts, data) values (?,?,?) USING TTL %d", table, ttl)
	for _, ig := range itergens {
		rowKey := fmt.Sprintf("%s_%d", id, ig.Ts/mdata.Month_sec)
		err := m.session.Query(query, rowKey, ig.Ts, mdata.PrepareChunkData(ig.Span, ig.Bytes())).Exec()
		if err != nil {
			throwError(fmt.Sprintf("Error in query: %q", err))
		}
	}
}

func throwError(msg string) {
	msg = fmt.Sprintf("%s\n", msg)
	printLock.Lock()
	fmt.Fprintln(os.Stderr, msg)
	printLock.Unlock()
}