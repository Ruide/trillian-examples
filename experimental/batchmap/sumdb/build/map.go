// Copyright 2020 Google LLC. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// map constructs a verifiable map from the modules in Go SumDB.
package main

import (
	"context"
	"crypto"
	"flag"
	"fmt"
	"reflect"

	"github.com/apache/beam/sdks/go/pkg/beam"
	"github.com/apache/beam/sdks/go/pkg/beam/io/databaseio"
	"github.com/apache/beam/sdks/go/pkg/beam/x/beamx"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"

	"github.com/google/trillian/experimental/batchmap"
	"github.com/google/trillian/experimental/batchmap/tilepb"
	"github.com/google/trillian/merkle/coniks"

	"github.com/google/trillian-examples/experimental/batchmap/sumdb/mapdb"

	_ "github.com/mattn/go-sqlite3"
)

const hash = crypto.SHA512_256

var (
	sumDB        = flag.String("sum_db", "", "The path of the SQLite file generated by sumdbaudit, e.g. ~/sum.db.")
	mapDB        = flag.String("map_db", "", "Output database where the map tiles will be written.")
	treeID       = flag.Int64("tree_id", 12345, "The ID of the tree. Used as a salt in hashing.")
	prefixStrata = flag.Int("prefix_strata", 2, "The number of strata of 8-bit strata before the final strata.")
	count        = flag.Int("count", -1, "The total number of entries starting from the beginning of the SumDB to use, or -1 to use all")
	batchSize    = flag.Int("write_batch_size", 250, "Number of tiles to write per batch")
)

func init() {
	beam.RegisterType(reflect.TypeOf((*mapEntryFn)(nil)).Elem())
	beam.RegisterType(reflect.TypeOf((*toMapDatabaseRowFn)(nil)).Elem())
}

// Metadata is the audit.Metadata object with the addition of an ID field.
// It must map to the scheme of the leafMetadata table.
type Metadata struct {
	ID       int64
	Module   string
	Version  string
	RepoHash string
	ModHash  string
}

// MapTile is the schema format of the Map database to allow for databaseio writing.
type MapTile struct {
	Revision int
	Path     []byte
	Tile     []byte
}

func main() {
	flag.Parse()
	beam.Init()

	if len(*sumDB) == 0 {
		glog.Exitf("No sum_db provided")
	}

	if len(*mapDB) == 0 {
		glog.Exitf("No map_db provided")
	}

	_, rev, err := sinkFromFlags()
	if err != nil {
		glog.Exitf("Failed to open map_db: %v", err)
	}

	p, s := beam.NewPipelineWithRoot()

	records := sourceFromFlags(s.Scope("source"))
	entries := beam.ParDo(s.Scope("mapentries"), &mapEntryFn{*treeID}, records)
	allTiles, err := batchmap.Create(s, entries, *treeID, hash, *prefixStrata)

	if err != nil {
		glog.Exitf("Failed to create pipeline: %q", err)
	}

	rows := beam.ParDo(s.Scope("convertoutput"), &toMapDatabaseRowFn{Revision: rev}, allTiles)
	databaseio.WriteWithBatchSize(s.Scope("sink"), *batchSize, "sqlite3", *mapDB, "tiles", []string{}, rows)

	// All of the above constructs the pipeline but doesn't run it. Now we run it.
	if err := beamx.Run(context.Background(), p); err != nil {
		glog.Exitf("Failed to execute job: %q", err)
	}
}

func sourceFromFlags(s beam.Scope) beam.PCollection {
	if *count < 0 {
		return databaseio.Read(s, "sqlite3", *sumDB, "leafMetadata", reflect.TypeOf(Metadata{}))
	}
	return databaseio.Query(s, "sqlite3", *sumDB, fmt.Sprintf("SELECT * FROM leafMetadata WHERE id < %d", *count), reflect.TypeOf(Metadata{}))
}

func sinkFromFlags() (*mapdb.TileDB, int, error) {
	tiledb, err := mapdb.NewTileDB(*mapDB)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to open map DB at %q: %v", *mapDB, err)
	}
	if err := tiledb.Init(); err != nil {
		return nil, 0, fmt.Errorf("failed to Init map DB at %q: %v", *mapDB, err)
	}

	var rev int
	if rev, err = tiledb.MaxRevision(); err != nil {
		switch err.(type) {
		case mapdb.NoRevisionsFound:
			return tiledb, 0, nil
		default:
			return nil, 0, fmt.Errorf("failed to query for max revision: %v", err)
		}
	} else {
		return tiledb, rev + 1, nil
	}
}

type mapEntryFn struct {
	TreeID int64
}

func (fn *mapEntryFn) ProcessElement(m Metadata, emit func(*tilepb.Entry)) {
	h := hash.New()
	h.Write([]byte(fmt.Sprintf("%s %s/go.mod", m.Module, m.Version)))
	modKey := h.Sum(nil)

	emit(&tilepb.Entry{
		HashKey:   modKey,
		HashValue: coniks.Default.HashLeaf(fn.TreeID, modKey, []byte(m.ModHash)),
	})

	h = hash.New()
	h.Write([]byte(fmt.Sprintf("%s %s", m.Module, m.Version)))
	repoKey := h.Sum(nil)

	emit(&tilepb.Entry{
		HashKey:   repoKey,
		HashValue: coniks.Default.HashLeaf(fn.TreeID, repoKey, []byte(m.RepoHash)),
	})
}

type toMapDatabaseRowFn struct {
	Revision int
}

func (fn *toMapDatabaseRowFn) ProcessElement(ctx context.Context, t *tilepb.Tile) (MapTile, error) {
	bs, err := proto.Marshal(t)
	if err != nil {
		return MapTile{}, err
	}
	return MapTile{
		Revision: fn.Revision,
		Path:     t.Path,
		Tile:     bs,
	}, nil
}
