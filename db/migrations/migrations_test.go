// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package migrations

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/mdbx"
	"github.com/erigontech/erigon/db/kv/memdb"
	"github.com/erigontech/erigon/db/rawdb"
)

// preHotTableStoreMigrations are the migrations a chaindata datadir created
// before kv.Eip8304Tables was added would already have applied. They are listed
// by name on purpose: renaming an old migration must fail the upgrade tests
// loudly rather than silently leaving a datadir unupgraded.
var preHotTableStoreMigrations = []string{
	"db_schema_version5",
	"reset_stage_txn_lookup",
	"db_schema_version6",
	"db_schema_version7",
	"drop_legacy_e2_tables",
}

// newTestMigrationsDB opens an in-memory migrations-tracking DB for use in tests.
func newTestMigrationsDB(t *testing.T) kv.RwDB {
	t.Helper()
	return memdb.NewTestDB(t, dbcfg.MigrationsDB)
}

func TestApplyWithInit(t *testing.T) {
	require, db := require.New(t), memdb.NewTestDB(t, dbcfg.ChainDB)
	migrationsDB := newTestMigrationsDB(t)
	m := []Migration{
		{
			"one",
			func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				tx, err := db.BeginRw(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback()

				if err := BeforeCommit(tx, nil, true); err != nil {
					return err
				}
				return tx.Commit()
			},
		},
		{
			"two",
			func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				tx, err := db.BeginRw(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback()

				if err := BeforeCommit(tx, nil, true); err != nil {
					return err
				}
				return tx.Commit()
			},
		},
	}

	migrator := NewMigrator(dbcfg.ChainDB)
	migrator.Migrations = m
	logger := log.New()
	err := migrator.Apply(db, migrationsDB, "", "", logger)
	require.NoError(err)
	var applied map[string][]byte
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied, err = AppliedMigrations(tx, false)
		require.NoError(err)

		_, ok := applied[m[0].Name]
		require.True(ok)
		_, ok = applied[m[1].Name]
		require.True(ok)
		return nil
	})
	require.NoError(err)

	// apply again
	err = migrator.Apply(db, migrationsDB, "", "", logger)
	require.NoError(err)
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied2, err := AppliedMigrations(tx, false)
		require.NoError(err)
		require.Equal(applied, applied2)
		return nil
	})
	require.NoError(err)
}

func TestApplyWithoutInit(t *testing.T) {
	require, db := require.New(t), memdb.NewTestDB(t, dbcfg.ChainDB)
	migrationsDB := newTestMigrationsDB(t)
	m := []Migration{
		{
			"one",
			func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				t.Fatal("shouldn't been executed")
				return nil
			},
		},
		{
			"two",
			func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				tx, err := db.BeginRw(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback()

				if err := BeforeCommit(tx, nil, true); err != nil {
					return err
				}
				return tx.Commit()
			},
		},
	}
	err := migrationsDB.Update(t.Context(), func(tx kv.RwTx) error {
		return tx.Put(kv.Migrations, []byte(m[0].Name), []byte{1})
	})
	require.NoError(err)

	migrator := NewMigrator(dbcfg.ChainDB)
	migrator.Migrations = m
	logger := log.New()
	err = migrator.Apply(db, migrationsDB, "", "", logger)
	require.NoError(err)

	var applied map[string][]byte
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied, err = AppliedMigrations(tx, false)
		require.NoError(err)

		require.Len(applied, 2)
		_, ok := applied[m[1].Name]
		require.True(ok)
		_, ok = applied[m[0].Name]
		require.True(ok)
		return nil
	})
	require.NoError(err)

	// apply again
	err = migrator.Apply(db, migrationsDB, "", "", logger)
	require.NoError(err)

	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied2, err := AppliedMigrations(tx, false)
		require.NoError(err)
		require.Equal(applied, applied2)
		return nil
	})
	require.NoError(err)

}

func TestWhenNonFirstMigrationAlreadyApplied(t *testing.T) {
	require, db := require.New(t), memdb.NewTestDB(t, dbcfg.ChainDB)
	migrationsDB := newTestMigrationsDB(t)
	m := []Migration{
		{
			"one",
			func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				tx, err := db.BeginRw(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback()

				if err := BeforeCommit(tx, nil, true); err != nil {
					return err
				}
				return tx.Commit()
			},
		},
		{
			"two",
			func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				t.Fatal("shouldn't been executed")
				return nil
			},
		},
	}
	err := migrationsDB.Update(t.Context(), func(tx kv.RwTx) error {
		return tx.Put(kv.Migrations, []byte(m[1].Name), []byte{1}) // apply non-first migration
	})
	require.NoError(err)

	migrator := NewMigrator(dbcfg.ChainDB)
	migrator.Migrations = m
	logger := log.New()
	err = migrator.Apply(db, migrationsDB, "", "", logger)
	require.NoError(err)

	var applied map[string][]byte
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied, err = AppliedMigrations(tx, false)
		require.NoError(err)

		require.Len(applied, 2)
		_, ok := applied[m[1].Name]
		require.True(ok)
		_, ok = applied[m[0].Name]
		require.True(ok)
		return nil
	})
	require.NoError(err)

	// apply again
	err = migrator.Apply(db, migrationsDB, "", "", logger)
	require.NoError(err)
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied2, err := AppliedMigrations(tx, false)
		require.NoError(err)
		require.Equal(applied, applied2)
		return nil
	})
	require.NoError(err)
}

// TestHotTableStoreBucketIsCoveredByAMigration pins the upgrade path for the
// bucket the hot table store writes to. ChaindataTables gained kv.Eip8304Tables,
// so a datadir that has already applied every migration of the previous release
// must still see one pending: that is what makes the node re-open the DB
// exclusively (creating the missing table) and record the new schema version.
// Without it, an accede-mode opener (rpcdaemon, integration tools) fails on such
// a datadir with "db-table doesn't exists: Eip8304Tables".
func TestHotTableStoreBucketIsCoveredByAMigration(t *testing.T) {
	require := require.New(t)
	db := memdb.NewTestDB(t, dbcfg.ChainDB)
	migrationsDB := newTestMigrationsDB(t)

	// Every migration that existed before the bucket was added.
	for _, name := range preHotTableStoreMigrations {
		require.NoError(migrationsDB.Update(t.Context(), func(tx kv.RwTx) error {
			return tx.Put(kv.Migrations, []byte(name), []byte("{}"))
		}))
	}

	migrator := NewMigrator(dbcfg.ChainDB)
	has, err := migrator.HasPendingMigrations(migrationsDB)
	require.NoError(err)
	require.True(has, "a datadir upgraded from the previous release must re-open for migrations")

	var pending []string
	require.NoError(migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		ms, err := migrator.PendingMigrations(tx)
		if err != nil {
			return err
		}
		for _, m := range ms {
			pending = append(pending, m.Name)
		}
		return nil
	}))
	require.Equal([]string{"db_schema_version8"}, pending)

	require.NoError(migrator.Apply(db, migrationsDB, t.TempDir(), "", log.New()))

	// The upgrade records the new schema version, which is what a reader checks
	// before touching the datadir.
	var major, minor uint32
	var ok bool
	require.NoError(db.View(t.Context(), func(tx kv.Tx) error {
		var err error
		major, minor, _, ok, err = rawdb.ReadDBSchemaVersion(tx)
		return err
	}))
	require.True(ok, "the schema version must be recorded")
	require.Equal(kv.DBSchemaVersion.Major, major)
	require.Equal(kv.DBSchemaVersion.Minor, minor)
}

// chaindataTablesWithoutHotStore builds the chaindata schema as it was before
// the EIP-8304 bucket was added, so a datadir created by the previous release
// can be reproduced.
func chaindataTablesWithoutHotStore(defaults kv.TableCfg) kv.TableCfg {
	out := kv.TableCfg{}
	for name, cfg := range defaults {
		if name != kv.Eip8304Tables {
			out[name] = cfg
		}
	}
	return out
}

// TestUpgradedDatadirStillOpensInAccedeMode is the end-to-end check behind
// dbSchemaVersion8: a datadir created before kv.Eip8304Tables existed must still
// be upgradeable, and after the upgrade an accede-mode opener (rpcdaemon,
// integration tools) must find the bucket. Without the migration the same open
// fails with "db-table doesn't exists: Eip8304Tables".
func TestUpgradedDatadirStillOpensInAccedeMode(t *testing.T) {
	dir := t.TempDir()
	chaindata := filepath.Join(dir, "chaindata")

	// 1. The pre-Week-14 datadir: no Eip8304Tables, schema version 7.0.
	old, err := mdbx.New(dbcfg.ChainDB, log.New()).Path(chaindata).WithTableCfg(chaindataTablesWithoutHotStore).Open(context.Background())
	if err != nil {
		t.Fatalf("create old datadir: %v", err)
	}
	if err := old.Update(context.Background(), func(tx kv.RwTx) error {
		var v [12]byte
		binary.BigEndian.PutUint32(v[0:], 7)
		binary.BigEndian.PutUint32(v[4:], 0)
		return tx.Put(kv.DatabaseInfo, kv.DBSchemaVersionKey, v[:])
	}); err != nil {
		t.Fatal(err)
	}
	old.Close()

	migrationsDB, err := OpenMigrationsDB(filepath.Join(dir, "migrations"), log.New())
	if err != nil {
		t.Fatal(err)
	}
	defer migrationsDB.Close()
	for _, name := range preHotTableStoreMigrations {
		if err := migrationsDB.Update(context.Background(), func(tx kv.RwTx) error {
			return tx.Put(kv.Migrations, []byte(name), []byte("{}"))
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 2. The node opens the datadir the way node.OpenDatabase does.
	db, err := mdbx.New(dbcfg.ChainDB, log.New()).Path(chaindata).Open(context.Background())
	if err != nil {
		t.Fatalf("node open: %v", err)
	}
	migrator := NewMigrator(dbcfg.ChainDB)
	has, err := migrator.HasPendingMigrations(migrationsDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pending migration after upgrade: %v", has)
	if !has {
		t.Fatal("no pending migration: the datadir would not be re-opened or re-versioned")
	}
	if err := migrator.Apply(db, migrationsDB, dir, chaindata, log.New()); err != nil {
		t.Fatalf("apply: %v", err)
	}
	db.Close()

	// 3. An accede-mode opener of the upgraded datadir.
	accede, err := mdbx.New(dbcfg.ChainDB, log.New()).Path(chaindata).Accede(true).Open(context.Background())
	if err != nil {
		t.Fatalf("accede open after the upgrade failed: %v", err)
	}
	defer accede.Close()
	if err := accede.View(context.Background(), func(tx kv.Tx) error {
		major, minor, _, ok, err := rawdb.ReadDBSchemaVersion(tx)
		if err != nil {
			return err
		}
		t.Logf("recorded schema version: %d.%d (present=%v)", major, minor, ok)
		if !ok || minor != kv.DBSchemaVersion.Minor {
			t.Errorf("datadir still reports %d.%d", major, minor)
		}
		_, err = tx.GetOne(kv.Eip8304Tables, []byte{0})
		return err
	}); err != nil {
		t.Fatalf("reading the new bucket: %v", err)
	}
}

func TestValidation(t *testing.T) {
	require, db := require.New(t), memdb.NewTestDB(t, dbcfg.ChainDB)
	migrationsDB := newTestMigrationsDB(t)
	m := []Migration{
		{
			Name: "repeated_name",
			Up: func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				tx, err := db.BeginRw(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback()

				if err := BeforeCommit(tx, nil, true); err != nil {
					return err
				}
				return tx.Commit()
			},
		},
		{
			Name: "repeated_name",
			Up: func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				tx, err := db.BeginRw(t.Context())
				if err != nil {
					return err
				}
				defer tx.Rollback()

				if err := BeforeCommit(tx, nil, true); err != nil {
					return err
				}
				return tx.Commit()
			},
		},
	}
	migrator := NewMigrator(dbcfg.ChainDB)
	migrator.Migrations = m
	logger := log.New()
	err := migrator.Apply(db, migrationsDB, "", "", logger)
	require.ErrorIs(err, ErrMigrationNonUniqueName)

	var applied map[string][]byte
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied, err = AppliedMigrations(tx, false)
		require.NoError(err)
		require.Empty(applied)
		return nil
	})
	require.NoError(err)
}

func TestCommitCallRequired(t *testing.T) {
	require, db := require.New(t), memdb.NewTestDB(t, dbcfg.ChainDB)
	migrationsDB := newTestMigrationsDB(t)
	m := []Migration{
		{
			Name: "one",
			Up: func(db kv.RwDB, dirs datadir.Dirs, progress []byte, BeforeCommit Callback, logger log.Logger) (err error) {
				//don't call BeforeCommit
				return nil
			},
		},
	}
	migrator := NewMigrator(dbcfg.ChainDB)
	migrator.Migrations = m
	logger := log.New()
	err := migrator.Apply(db, migrationsDB, "", "", logger)
	require.ErrorIs(err, ErrMigrationCommitNotCalled)

	var applied map[string][]byte
	err = migrationsDB.View(t.Context(), func(tx kv.Tx) error {
		applied, err = AppliedMigrations(tx, false)
		require.NoError(err)
		require.Empty(applied)
		return nil
	})
	require.NoError(err)
}
