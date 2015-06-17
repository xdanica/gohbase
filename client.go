// Copyright (C) 2015  The GoHBase Authors.  All rights reserved.
// This file is part of GoHBase.
// Use of this source code is governed by the Apache License 2.0
// that can be found in the COPYING file.

package gohbase

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"

	"github.com/cznic/b"
	"github.com/golang/protobuf/proto"
	"github.com/tsuna/gohbase/hrpc"
	"github.com/tsuna/gohbase/pb"
	"github.com/tsuna/gohbase/region"
	"github.com/tsuna/gohbase/zk"
	"golang.org/x/net/context"
)

// Constants
var (
	// Name of the meta region.
	metaTableName = []byte("hbase:meta")

	metaRegionInfo = &region.Info{
		Table:      []byte("hbase:meta"),
		RegionName: []byte("hbase:meta,,1"),
		StopKey:    []byte{},
	}

	infoFamily = map[string][]string{
		"info": nil,
	}

	// ErrDeadline is returned when the deadline of a request has been exceeded
	ErrDeadline = errors.New("deadline exceeded")
)

// region -> client cache.
type regionClientCache struct {
	m sync.Mutex

	clients map[*region.Info]*region.Client
}

func (rcc *regionClientCache) get(r *region.Info) *region.Client {
	rcc.m.Lock()
	c := rcc.clients[r]
	rcc.m.Unlock()
	return c
}

func (rcc *regionClientCache) put(r *region.Info, c *region.Client) {
	rcc.m.Lock()
	rcc.clients[r] = c
	rcc.m.Unlock()
}

// key -> region cache.
type keyRegionCache struct {
	m sync.Mutex

	// Maps a []byte of a region start key to a *region.Info
	regions *b.Tree
}

func (krc *keyRegionCache) get(key []byte) ([]byte, *region.Info) {
	// When seeking - "The Enumerator's position is possibly after the last item in the tree"
	// http://godoc.org/github.com/cznic/b#Tree.Set
	krc.m.Lock()
	enum, ok := krc.regions.Seek(key)
	k, v, err := enum.Prev()
	if err == io.EOF && krc.regions.Len() > 0 {
		// We're past the end of the tree. Return the last element instead.
		// (Without this code we always get a cache miss and create a new client for each req.)
		k, v = krc.regions.Last()
		err = nil
	} else if !ok {
		k, v, err = enum.Prev()
	}
	// TODO: It would be nice if we could do just enum.Get() to avoid the
	// unnecessary cost of seeking to the next entry.
	krc.m.Unlock()
	if err != nil {
		return nil, nil
	}
	return k.([]byte), v.(*region.Info)
}

func (krc *keyRegionCache) put(key []byte, reg *region.Info) *region.Info {
	krc.m.Lock()
	oldV, _ := krc.regions.Put(key, func(interface{}, bool) (interface{}, bool) { return reg, true })
	krc.m.Unlock()
	if oldV == nil {
		return nil
	}
	return oldV.(*region.Info)
}

// A Client provides access to an HBase cluster.
type Client struct {
	regions keyRegionCache

	// Maps a *region.Info to the *region.Client that we think currently
	// serves it.
	clients regionClientCache

	// Client connected to the RegionServer hosting the hbase:meta table.
	metaClient *region.Client

	zkquorum string
}

// NewClient creates a new HBase client.
func NewClient(zkquorum string) *Client {
	return &Client{
		regions:  keyRegionCache{regions: b.TreeNew(region.CompareGeneric)},
		clients:  regionClientCache{clients: make(map[*region.Info]*region.Client)},
		zkquorum: zkquorum,
	}
}

// CheckTable returns an error if the given table name doesn't exist.
func (c *Client) CheckTable(table string) (*pb.GetResponse, error) {
	resp, err := c.sendRPC(hrpc.NewGetStr(context.Background(), table, "theKey", nil))
	if err != nil {
		return nil, err
	}
	return resp.(*pb.GetResponse), err
}

// Get returns a single row fetched from HBase.
func (c *Client) Get(ctx context.Context, table, rowkey string, families map[string][]string) (*pb.GetResponse, error) {
	resp, err := c.sendRPC(hrpc.NewGetStr(ctx, table, rowkey, families))
	if err != nil {
		return nil, err
	}
	return resp.(*pb.GetResponse), err
}

// Scan retrieves the values specified in families from the given range.
func (c *Client) Scan(ctx context.Context, table string, families map[string][]string, startRow, stopRow []byte) ([]*pb.Result, error) {
	var results []*pb.Result
	var scanres *pb.ScanResponse
	var rpc *hrpc.Scan

	for {
		// Make a new Scan RPC for this region
		if rpc == nil {
			// If it's the first region, just begin at the given startRow
			rpc = hrpc.NewScanStr(ctx, table, families, startRow, stopRow)
		} else {
			// If it's not the first region, we want to start at whatever the
			// last region's StopKey was
			rpc = hrpc.NewScanStr(ctx, table, families, rpc.GetRegionStop(), stopRow)
		}

		res, err := c.sendRPC(rpc)
		if err != nil {
			return nil, err
		}
		scanres = res.(*pb.ScanResponse)
		results = append(results, scanres.Results...)

		// TODO: The more_results field of the ScanResponse object was always
		// true, so we should figure out if there's a better way to know when
		// to move on to the next region than making an extra request and
		// seeing if there were no results
		for len(scanres.Results) != 0 {
			rpc = hrpc.NewScanFromID(ctx, table, *scanres.ScannerId, rpc.Key())

			res, err = c.sendRPC(rpc)
			if err != nil {
				return nil, err
			}
			scanres = res.(*pb.ScanResponse)
			results = append(results, scanres.Results...)
		}

		rpc = hrpc.NewCloseFromID(ctx, table, *scanres.ScannerId, rpc.Key())
		if err != nil {
			return nil, err
		}
		res, err = c.sendRPC(rpc)

		// Check to see if this region is the last we should scan (either
		// because (1) it's the last region or (3) because its stop_key is
		// greater than or equal to the stop_key of this scanner provided
		// that (2) we're not trying to scan until the end of the table).
		// (1)                               (2)                  (3)
		if len(rpc.GetRegionStop()) == 0 || (len(stopRow) != 0 && bytes.Compare(stopRow, rpc.GetRegionStop()) <= 0) {
			return results, nil
		}
	}
}

// Put inserts or updates the values into the given row of the table.
func (c *Client) Put(ctx context.Context, table string, rowkey string, values map[string]map[string][]byte) (*pb.MutateResponse, error) {
	resp, err := c.sendRPC(hrpc.NewPutStr(ctx, table, rowkey, values))
	if err != nil {
		return nil, err
	}
	return resp.(*pb.MutateResponse), err
}

// Delete removes values from the given row of the table.
func (c *Client) Delete(ctx context.Context, table, rowkey string, values map[string]map[string][]byte) (*pb.MutateResponse, error) {
	resp, err := c.sendRPC(hrpc.NewDelStr(ctx, table, rowkey, values))
	if err != nil {
		return nil, err
	}
	return resp.(*pb.MutateResponse), err
}

// Append atomically appends all the given values to their current values in HBase.
func (c *Client) Append(ctx context.Context, table, rowkey string, values map[string]map[string][]byte) (*pb.MutateResponse, error) {
	resp, err := c.sendRPC(hrpc.NewAppStr(ctx, table, rowkey, values))
	if err != nil {
		return nil, err
	}
	return resp.(*pb.MutateResponse), err
}

// Increment atomically increments the given values in HBase.
func (c *Client) Increment(ctx context.Context, table, rowkey string, values map[string]map[string][]byte) (*pb.MutateResponse, error) {
	resp, err := c.sendRPC(hrpc.NewIncStr(ctx, table, rowkey, values))
	if err != nil {
		return nil, err
	}
	return resp.(*pb.MutateResponse), err
}

// Creates the META key to search for in order to locate the given key.
func createRegionSearchKey(table, key []byte) []byte {
	metaKey := make([]byte, 0, len(table)+len(key)+3)
	metaKey = append(metaKey, table...)
	metaKey = append(metaKey, ',')
	metaKey = append(metaKey, key...)
	metaKey = append(metaKey, ',')
	// ':' is the first byte greater than '9'.  We always want to find the
	// entry with the greatest timestamp, so by looking right before ':'
	// we'll find it.
	metaKey = append(metaKey, ':')
	return metaKey
}

// Checks whether or not the given cache key is for the given table.
func isCacheKeyForTable(table, cacheKey []byte) bool {
	// Check we found an entry that's really for the requested table.
	for i := 0; i < len(table); i++ {
		if table[i] != cacheKey[i] { // This table isn't in the map, we found
			return false // a key which is for another table.
		}
	}

	// Make sure we didn't find another key that's for another table
	// whose name is a prefix of the table name we were given.
	return cacheKey[len(table)] == ','
}

// Searches in the regions cache for the region hosting the given row.
func (c *Client) getRegion(table, key []byte) *region.Info {
	if bytes.Equal(table, metaTableName) {
		return metaRegionInfo
	}
	regionName := createRegionSearchKey(table, key)
	region_key, region := c.regions.get(regionName)
	if region == nil || !isCacheKeyForTable(table, region_key) {
		return nil
	}

	if len(region.StopKey) != 0 &&
		// If the stop key is an empty byte array, it means this region is the
		// last region for this table and this key ought to be in that region.
		bytes.Compare(key, region.StopKey) >= 0 {
		return nil
	}

	return region
}

// Returns the client currently known to hose the given region, or NULL.
func (c *Client) clientFor(region *region.Info) *region.Client {
	if region == metaRegionInfo {
		return c.metaClient
	}
	return c.clients.get(region)
}

// Queues an RPC targeted at a particular region for handling by the appropriate
// region client. Results will be written to the rpc's result and error
// channels.
func (c *Client) queueRPC(rpc hrpc.Call) error {
	table := rpc.Table()
	key := rpc.Key()
	reg := c.getRegion(table, key)

	var client *region.Client
	if reg != nil {
		client = c.clientFor(reg)
	} else {
		var err error
		client, reg, err = c.locateRegion(table, key)
		if err != nil {
			return err
		}
	}
	rpc.SetRegion(reg.RegionName, reg.StopKey)
	client.QueueRPC(rpc)
	return nil
}

func (c *Client) sendRPC(rpc hrpc.Call) (proto.Message, error) {
	resch := rpc.GetResultChan()
	err := c.queueRPC(rpc)
	if err != nil {
		return nil, err
	}

	select {
	case res := <-resch:
		return res.Msg, res.Error
	case <-rpc.Context().Done():
		return nil, ErrDeadline
	}
}

// Locates the region in which the given row key for the given table is.
func (c *Client) locateRegion(table, key []byte) (*region.Client, *region.Info, error) {
	if c.metaClient == nil {
		err := c.locateMeta()
		if err != nil {
			return nil, nil, err
		}
	}
	metaKey := createRegionSearchKey(table, key)
	rpc := hrpc.NewGetBefore(context.Background(), metaTableName, metaKey, infoFamily)
	rpc.SetRegion(metaRegionInfo.RegionName, metaRegionInfo.StopKey)
	resp, err := c.metaClient.SendRPC(rpc)
	if err != nil {
		return nil, nil, err
	}
	return c.discoverRegion(resp.(*pb.GetResponse))
}

// For dependency injection in tests.
var newRegion = func(host string, port uint16) (*region.Client, error) {
	return region.NewClient(host, port)
}

// Adds a new region to our regions cache.
func (c *Client) discoverRegion(metaRow *pb.GetResponse) (*region.Client, *region.Info, error) {
	if metaRow.Result == nil {
		return nil, nil, errors.New("table not found")
	}
	var host string
	var port uint16
	var reg *region.Info
	for _, cell := range metaRow.Result.Cell {
		switch string(cell.Qualifier) {
		case "regioninfo":
			var err error
			reg, err = region.InfoFromCell(cell)
			if err != nil {
				return nil, nil, err
			}
		case "server":
			value := cell.Value
			if len(value) == 0 {
				continue // Empty during NSRE.
			}
			colon := bytes.IndexByte(value, ':')
			if colon < 1 { // Colon can't be at the beginning.
				return nil, nil,
					fmt.Errorf("broken meta: no colon found in info:server %q", cell)
			}
			host = string(value[:colon])
			portU64, err := strconv.ParseUint(string(value[colon+1:]), 10, 16)
			if err != nil {
				return nil, nil, err
			}
			port = uint16(portU64)
		default:
			// Other kinds of qualifiers: ignore them.
			// TODO: If this is the parent of a split region, there are two other
			// KVs that could be useful: `info:splitA' and `info:splitB'.
			// Need to investigate whether we can use those as a hint to update our
			// regions_cache with the daughter regions of the split.
		}
	}

	client, err := newRegion(host, port)
	if err != nil {
		return nil, nil, err
	}

	c.addRegionToCache(reg, client)

	return client, reg, nil
}

// Adds a region to our meta cache.
func (c *Client) addRegionToCache(reg *region.Info, client *region.Client) {
	// 1. Record the region -> client mapping.
	// This won't be "discoverable" until another map points to it, because
	// at this stage no one knows about this region yet, so another thread
	// may be looking up that region again while we're in the process of
	// publishing our findings.
	c.clients.put(reg, client)

	// 2. Store the region in the sorted map.
	// This will effectively "publish" the result of our work to other
	// threads.  The window between when the previous `put' becomes visible
	// to all other threads and when we're done updating the sorted map is
	// when we may unnecessarily re-lookup the same region again.  It's an
	// acceptable trade-off.  We avoid extra synchronization complexity in
	// exchange of occasional duplicate work (which should be rare anyway).
	c.regions.put(reg.RegionName, reg)
}

// Looks up the meta region in ZooKeeper.
func (c *Client) locateMeta() error {
	host, port, err := zk.LocateMeta(c.zkquorum)
	if err != nil {
		log.Printf("Error while locating meta: %s", err)
		return err
	}
	log.Printf("Meta @ %s:%d", host, port)
	c.metaClient, err = region.NewClient(host, port)
	return err
}