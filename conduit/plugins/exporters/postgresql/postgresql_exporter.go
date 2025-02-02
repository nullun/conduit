package postgresql

import (
	"context"
	_ "embed" // used to embed config
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	sdk "github.com/algorand/go-algorand-sdk/v2/types"
	"github.com/algorand/indexer/idb"
	_ "github.com/algorand/indexer/idb/postgres" // register driver
	"github.com/algorand/indexer/types"
	iutil "github.com/algorand/indexer/util"

	"github.com/algorand/conduit/conduit"
	"github.com/algorand/conduit/conduit/data"
	"github.com/algorand/conduit/conduit/plugins"
	"github.com/algorand/conduit/conduit/plugins/exporters"
	"github.com/algorand/conduit/conduit/plugins/exporters/postgresql/util"
)

// PluginName to use when configuring.
const PluginName = "postgresql"

var errMissingDelta = errors.New("ledger state delta is missing from block, ensure algod importer is using 'follower' mode")

type postgresqlExporter struct {
	round  uint64
	cfg    ExporterConfig
	db     idb.IndexerDb
	logger *logrus.Logger
	wg     sync.WaitGroup
	ctx    context.Context
	cf     context.CancelFunc
	dm     util.DataManager
}

//go:embed sample.yaml
var sampleConfig string

var metadata = conduit.Metadata{
	Name:         PluginName,
	Description:  "Exporter for writing data to a postgresql instance.",
	Deprecated:   false,
	SampleConfig: sampleConfig,
}

func (exp *postgresqlExporter) Metadata() conduit.Metadata {
	return metadata
}

func (exp *postgresqlExporter) Init(ctx context.Context, initProvider data.InitProvider, cfg plugins.PluginConfig, logger *logrus.Logger) error {
	exp.ctx, exp.cf = context.WithCancel(ctx)
	dbName := "postgres"
	exp.logger = logger
	if err := cfg.UnmarshalConfig(&exp.cfg); err != nil {
		return fmt.Errorf("connect failure in unmarshalConfig: %v", err)
	}
	// Inject a dummy db for unit testing
	if exp.cfg.Test {
		dbName = "dummy"
	}
	var opts idb.IndexerDbOptions
	opts.MaxConn = exp.cfg.MaxConn
	opts.ReadOnly = false

	// for some reason when ConnectionString is empty, it's automatically
	// connecting to a local instance that's running.
	// this behavior can be reproduced in TestConnectDbFailure.
	if !exp.cfg.Test && exp.cfg.ConnectionString == "" {
		return fmt.Errorf("connection string is empty for %s", dbName)
	}
	db, ready, err := idb.IndexerDbByName(dbName, exp.cfg.ConnectionString, opts, exp.logger)
	if err != nil {
		return fmt.Errorf("connect failure constructing db, %s: %v", dbName, err)
	}
	exp.db = db
	<-ready
	_, err = iutil.EnsureInitialImport(exp.db, *initProvider.GetGenesis())
	if err != nil {
		return fmt.Errorf("error importing genesis: %v", err)
	}
	dbRound, err := db.GetNextRoundToAccount()
	if err != nil {
		return fmt.Errorf("error getting next db round : %v", err)
	}
	if uint64(initProvider.NextDBRound()) != dbRound {
		return fmt.Errorf("initializing block round %d but next round to account is %d", initProvider.NextDBRound(), dbRound)
	}
	exp.round = uint64(initProvider.NextDBRound())

	// if data pruning is enabled
	if !exp.cfg.Test && exp.cfg.Delete.Rounds > 0 {
		exp.dm = util.MakeDataManager(exp.ctx, &exp.cfg.Delete, exp.db, logger)
		exp.wg.Add(1)
		go exp.dm.DeleteLoop(&exp.wg, &exp.round)
	}
	return nil
}

func (exp *postgresqlExporter) Config() string {
	ret, _ := yaml.Marshal(exp.cfg)
	return string(ret)
}

func (exp *postgresqlExporter) Close() error {
	if exp.db != nil {
		exp.db.Close()
	}

	exp.cf()
	exp.wg.Wait()
	return nil
}

func (exp *postgresqlExporter) Receive(exportData data.BlockData) error {
	if exportData.Delta == nil {
		if exportData.Round() == 0 {
			exportData.Delta = &sdk.LedgerStateDelta{}
		} else {
			return errMissingDelta
		}
	}
	vb := types.ValidatedBlock{
		Block: sdk.Block{BlockHeader: exportData.BlockHeader, Payset: exportData.Payset},
		Delta: *exportData.Delta,
	}
	if err := exp.db.AddBlock(&vb); err != nil {
		return err
	}
	atomic.StoreUint64(&exp.round, exportData.Round()+1)
	return nil
}

func init() {
	exporters.Register(PluginName, exporters.ExporterConstructorFunc(func() exporters.Exporter {
		return &postgresqlExporter{}
	}))
}
