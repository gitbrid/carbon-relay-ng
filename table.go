package main

import (
	"errors"
	"fmt"
	"github.com/Dieterbe/go-metrics"
	"github.com/graphite-ng/carbon-relay-ng/aggregator"
	"sync"
)

type Table struct {
	sync.Mutex
	Blacklist     []*Matcher               `json:"blacklist"`
	Routes        []*Route                 `json:"routes"`
	Aggregators   []*aggregator.Aggregator `json:"aggregators"`
	spoolDir      string
	numBlacklist  metrics.Counter
	numUnroutable metrics.Counter
	In            chan []byte `json:"-"` // channel api to trade in some performance for encapsulation
}

func NewTable(spoolDir string) *Table {
	routes := make([]*Route, 0)
	aggregators := make([]*aggregator.Aggregator, 0)
	blacklist := make([]*Matcher, 0)
	t := &Table{
		sync.Mutex{},
		blacklist,
		routes,
		aggregators,
		spoolDir,
		Counter("unit=Metric.direction=blacklist"),
		Counter("unit=Metric.direction=unroutable"),
		make(chan []byte),
	}
	go func() {
		for buf := range t.In {
			t.Dispatch(buf)
		}
	}()
	return t
}

// buf is assumed to have no whitespace at the end
func (table *Table) Dispatch(buf []byte) {
	table.Lock()
	defer table.Unlock()

	for _, matcher := range table.Blacklist {
		if matcher.Match(buf) {
			table.numBlacklist.Inc(1)
			return
		}
	}

	for _, aggregator := range table.Aggregators {
		aggregator.In <- buf
	}

	routed := false

	for _, route := range table.Routes {
		if route.Match(buf) {
			routed = true
			//fmt.Println("routing to " + dest.Key)
			// routes should take this in as fast as they can
			log.Info("table sending to route: %s", buf)
			route.in <- buf
		}
	}

	if !routed {
		table.numUnroutable.Inc(1)
		log.Notice("unrouteable: %s\n", buf)
	}

}

// to view the state of the table/route at any point in time
// we might add more functions to view specific entries if the need for that appears
func (table *Table) Snapshot() *Table {

	table.Lock()
	defer table.Unlock()

	blacklist := make([]*Matcher, len(table.Blacklist))
	for i, p := range table.Blacklist {
		blacklist[i] = p
	}

	routes := make([]*Route, len(table.Routes))
	for i, r := range table.Routes {
		routes[i] = r.Snapshot()
	}

	aggs := make([]*aggregator.Aggregator, len(table.Aggregators))
	for i, a := range table.Aggregators {
		aggs[i] = a.Snapshot()
	}
	return &Table{sync.Mutex{}, blacklist, routes, aggs, table.spoolDir, nil, nil, nil}
}

func (table *Table) GetRoute(key string) *Route {
	table.Lock()
	defer table.Unlock()
	for _, r := range table.Routes {
		if r.Key == key {
			return r
		}
	}
	return nil
}

// AddRoute adds a route to the table.
// The Route must be running already
func (table *Table) AddRoute(route *Route) {
	table.Lock()
	defer table.Unlock()
	table.Routes = append(table.Routes, route)
}

func (table *Table) AddBlacklist(matcher *Matcher) {
	table.Lock()
	defer table.Unlock()
	table.Blacklist = append(table.Blacklist, matcher)
}

func (table *Table) AddAggregator(agg *aggregator.Aggregator) {
	table.Lock()
	defer table.Unlock()
	table.Aggregators = append(table.Aggregators, agg)
}

func (table *Table) DelAggregator(id int) error {
	table.Lock()
	defer table.Unlock()
	fmt.Println("deleting", id)

	if id >= len(table.Aggregators) {
		return errors.New("Not found")
	}

	agg := table.Aggregators[id]
	fmt.Println("len", len(table.Aggregators))
	table.Aggregators = append(table.Aggregators[:id], table.Aggregators[id+1:]...)
	fmt.Println("len", len(table.Aggregators))
	agg.Shutdown()
	return nil
}

func (table *Table) Flush() error {
	table.Lock()
	defer table.Unlock()
	for _, route := range table.Routes {
		err := route.Flush()
		if err != nil {
			return err
		}
	}
	return nil
}

func (table *Table) Shutdown() error {
	table.Lock()
	defer table.Unlock()
	for _, route := range table.Routes {
		err := route.Shutdown()
		if err != nil {
			return err
		}
	}
	table.Routes = make([]*Route, 0)
	return nil
}

// idempotent semantics, not existing is fine
func (table *Table) DelRoute(key string) error {
	table.Lock()
	defer table.Unlock()
	toDelete := -1
	var i int
	var route *Route
	for i, route = range table.Routes {
		if route.Key == key {
			toDelete = i
			break
		}
	}
	if toDelete == -1 {
		return nil
	}

	table.Routes = append(table.Routes[:toDelete], table.Routes[toDelete+1:]...)

	err := route.Shutdown()
	if err != nil {
		// dest removed from routing table but still trying to connect
		// it won't get new stuff on its input though
		return err
	}
	return nil
}

func (table *Table) DelBlacklist(index int) error {
	table.Lock()
	defer table.Unlock()
	if index >= len(table.Blacklist) {
		return errors.New(fmt.Sprintf("Invalid index %d", index))
	}
	table.Blacklist = append(table.Blacklist[:index], table.Blacklist[index+1:]...)
	return nil
}

func (table *Table) DelDestination(key string, index int) error {
	route := table.GetRoute(key)
	if route == nil {
		return errors.New(fmt.Sprintf("Invalid route for %v", key))
	}
	return route.DelDestination(index)
}

func (table *Table) UpdateDestination(key string, index int, opts map[string]string) error {
	route := table.GetRoute(key)
	if route == nil {
		return errors.New(fmt.Sprintf("Invalid route for %v", key))
	}
	return route.UpdateDestination(index, opts)
}

func (table *Table) UpdateRoute(key string, opts map[string]string) error {
	route := table.GetRoute(key)
	if route == nil {
		return errors.New(fmt.Sprintf("Invalid route for %v", key))
	}
	return route.Update(opts)
}

func (table *Table) Print() (str string) {
	// TODO also print route type, print blacklist
	// we want to print things concisely (but no smaller than the defaults below)
	// so we have to figure out the max lengths of everything first
	// the default values can be arbitrary (bot not smaller than the column titles),
	// i figured multiples of 4 should look good
	// 'R' stands for Route, 'D' for dest, 'B' blacklist, 'A" for aggregation
	maxBPrefix := 4
	maxBSub := 4
	maxBRegex := 4
	maxAFunc := 4
	maxARegex := 8
	maxAOutFmt := 8
	maxAInterval := 4
	maxAwait := 4
	maxRKey := 8
	maxRPrefix := 4
	maxRSub := 4
	maxRRegex := 4
	maxDPrefix := 4
	maxDSub := 4
	maxDRegex := 4
	maxDAddr := 16
	maxDSpoolDir := 16

	t := table.Snapshot()
	for _, black := range t.Blacklist {
		maxBPrefix = max(maxBRegex, len(black.Prefix))
		maxBSub = max(maxBSub, len(black.Sub))
		maxBRegex = max(maxBRegex, len(black.Regex))
	}
	for _, agg := range t.Aggregators {
		maxAFunc = max(maxAFunc, len(agg.Fun))
		maxARegex = max(maxARegex, len(agg.Regex))
		maxAOutFmt = max(maxAOutFmt, len(agg.OutFmt))
		maxAInterval = max(maxAInterval, len(fmt.Sprintf("%d", agg.Interval)))
		maxAwait = max(maxAwait, len(fmt.Sprintf("%d", agg.Wait)))
	}
	for _, route := range t.Routes {
		maxRKey = max(maxRKey, len(route.Key))
		maxRPrefix = max(maxRPrefix, len(route.Matcher.Prefix))
		maxRSub = max(maxRSub, len(route.Matcher.Sub))
		maxRRegex = max(maxRRegex, len(route.Matcher.Regex))
		for _, dest := range route.Dests {
			maxDPrefix = max(maxDPrefix, len(dest.Matcher.Prefix))
			maxDSub = max(maxDSub, len(dest.Matcher.Sub))
			maxDRegex = max(maxDRegex, len(dest.Matcher.Regex))
			maxDAddr = max(maxDAddr, len(dest.Addr))
			maxDSpoolDir = max(maxDSpoolDir, len(dest.spoolDir))
		}
	}
	heaFmtB := fmt.Sprintf("%%%ds %%%ds %%%ds\n", maxBPrefix+1, maxBSub+1, maxBRegex+1)
	rowFmtB := fmt.Sprintf("%%%ds %%%ds %%%ds\n", maxBPrefix+1, maxBSub+1, maxBRegex+1)
	heaFmtA := fmt.Sprintf("%%%ds %%%ds %%%ds %%%ds %%%ds\n", maxAFunc+1, maxARegex+1, maxAOutFmt+1, maxAInterval+1, maxAwait+1)
	rowFmtA := fmt.Sprintf("%%%ds %%%ds %%%ds %%%dd %%%dd\n", maxAFunc+1, maxARegex+1, maxAOutFmt+1, maxAInterval+1, maxAwait+1)
	heaFmtR := fmt.Sprintf("  %%%ds %%%ds %%%ds %%%ds\n", maxRKey+1, maxRPrefix+1, maxRSub+1, maxRRegex+1)
	rowFmtR := fmt.Sprintf("> %%%ds %%%ds %%%ds %%%ds\n", maxRKey+1, maxRPrefix+1, maxRSub+1, maxRRegex+1)
	heaFmtD := fmt.Sprintf("        %%%ds %%%ds %%%ds %%%ds %%%ds %%6s %%6s %%6s\n", maxDPrefix+1, maxDSub+1, maxDRegex+1, maxDAddr+1, maxDSpoolDir+1)
	rowFmtD := fmt.Sprintf("                %%%ds %%%ds %%%ds %%%ds %%%ds %%6t %%6t %%6t\n", maxDPrefix+1, maxDSub+1, maxDRegex+1, maxDAddr+1, maxDSpoolDir+1)

	underscore := func(amount int) string {
		str := ""
		for i := 1; i < amount; i++ {
			str += "="
		}
		str += "\n"
		return str
	}

	str += "\n## Blacklist:\n"
	cols := fmt.Sprintf(heaFmtB, "prefix", "substr", "regex")
	str += cols + underscore(len(cols))
	for _, black := range t.Blacklist {
		str += fmt.Sprintf(rowFmtB, black.Prefix, black.Sub, black.Regex)
	}

	str += "\n## Aggregations:\n"
	cols = fmt.Sprintf(heaFmtA, "func", "regex", "outFmt", "interval", "wait")
	str += cols + underscore(len(cols))
	for _, agg := range t.Aggregators {
		str += fmt.Sprintf(rowFmtA, agg.Fun, agg.Regex, agg.OutFmt, agg.Interval, agg.Wait)
	}

	str += "\n## Routes:\n"
	cols = fmt.Sprintf(heaFmtR, "key", "prefix", "substr", "regex")
	str += cols + underscore(len(cols))

	for _, route := range t.Routes {
		m := route.Matcher
		str += fmt.Sprintf(rowFmtR, route.Key, m.Prefix, m.Sub, m.Regex)
		str += fmt.Sprintf(heaFmtD, "prefix", "substr", "regex", "addr", "spoolDir", "spool", "pickle", "online")
		str += "              "
		for i := 1; i < maxDPrefix+maxDSub+maxDRegex+maxDAddr+maxDSpoolDir+5+3*6+10; i++ {
			str += "-"
		}
		str += "\n"
		for _, dest := range route.Dests {
			m := dest.Matcher
			str += fmt.Sprintf(rowFmtD, m.Prefix, m.Sub, m.Regex, dest.Addr, dest.spoolDir, dest.Spool, dest.Pickle, dest.Online)
		}
		str += "\n"
	}
	return
}
