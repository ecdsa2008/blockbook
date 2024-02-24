package main

import (
	"context"
	"flag"
	"log"
	"math/rand"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang/glog"
	"github.com/juju/errors"
	"github.com/trezor/blockbook/api"
	"github.com/trezor/blockbook/bchain"
	"github.com/trezor/blockbook/bchain/coins"
	"github.com/trezor/blockbook/common"
	"github.com/trezor/blockbook/db"
	"github.com/trezor/blockbook/fiat"
	"github.com/trezor/blockbook/fourbyte"
	"github.com/trezor/blockbook/server"
)

// debounce too close requests for resync
const debounceResyncIndexMs = 1009

// debounce too close requests for resync mempool (ZeroMQ sends message for each tx, when new block there are many transactions)
const debounceResyncMempoolMs = 1009

// store internal state about once every minute
const storeInternalStatePeriodMs = 59699

// exit codes from the main function
const exitCodeOK = 0
const exitCodeFatal = 255

var (
	configFile = flag.String("blockchaincfg", "", "path to blockchain RPC service configuration json file")

	dbPath         = flag.String("datadir", "./data", "path to database directory")
	dbCache        = flag.Int("dbcache", 1<<29, "size of the rocksdb cache")
	dbMaxOpenFiles = flag.Int("dbmaxopenfiles", 1<<14, "max open files by rocksdb")

	blockFrom      = flag.Int("blockheight", -1, "height of the starting block")
	blockUntil     = flag.Int("blockuntil", -1, "height of the final block")
	rollbackHeight = flag.Int("rollback", -1, "rollback to the given height and quit")

	synchronize = flag.Bool("sync", false, "synchronizes until tip, if together with zeromq, keeps index synchronized")
	repair      = flag.Bool("repair", false, "repair the database")
	fixUtxo     = flag.Bool("fixutxo", false, "check and fix utxo db and exit")
	prof        = flag.String("prof", "", "http server binding [address]:port of the interface to profiling data /debug/pprof/ (default no profiling)")

	syncChunk   = flag.Int("chunk", 100, "block chunk size for processing in bulk mode")
	syncWorkers = flag.Int("workers", 8, "number of workers to process blocks in bulk mode")
	dryRun      = flag.Bool("dryrun", false, "do not index blocks, only download")

	debugMode = flag.Bool("debug", false, "debug mode, return more verbose errors, reload templates on each request")

	internalBinding = flag.String("internal", "", "internal http server binding [address]:port, (default no internal server)")

	publicBinding = flag.String("public", "", "public http server binding [address]:port[/path] (default no public server)")

	certFiles = flag.String("certfile", "", "to enable SSL specify path to certificate files without extension, expecting <certfile>.crt and <certfile>.key (default no SSL)")

	explorerURL = flag.String("explorer", "", "address of blockchain explorer")

	noTxCache = flag.Bool("notxcache", false, "disable tx cache")

	enableSubNewTx = flag.Bool("enablesubnewtx", false, "enable support for subscribing to all new transactions")

	computeColumnStats  = flag.Bool("computedbstats", false, "compute column stats and exit")
	computeFeeStatsFlag = flag.Bool("computefeestats", false, "compute fee stats for blocks in blockheight-blockuntil range and exit")
	dbStatsPeriodHours  = flag.Int("dbstatsperiod", 24, "period of db stats collection in hours, 0 disables stats collection")

	// resync index at least each resyncIndexPeriodMs (could be more often if invoked by message from ZeroMQ)
	resyncIndexPeriodMs = flag.Int("resyncindexperiod", 935093, "resync index period in milliseconds")

	// resync mempool at least each resyncMempoolPeriodMs (could be more often if invoked by message from ZeroMQ)
	resyncMempoolPeriodMs = flag.Int("resyncmempoolperiod", 60017, "resync mempool period in milliseconds")

	extendedIndex = flag.Bool("extendedindex", false, "if true, create index of input txids and spending transactions")
)
var (
	chanSyncIndex                 = make(chan struct{})
	chanSyncMempool               = make(chan struct{})
	chanStoreInternalState        = make(chan struct{})
	chanSyncIndexDone             = make(chan struct{})
	chanSyncMempoolDone           = make(chan struct{})
	chanStoreInternalStateDone    = make(chan struct{})
	chain                         bchain.BlockChain
	mempool                       bchain.Mempool
	index                         *db.RocksDB
	txCache                       *db.TxCache
	metrics                       *common.Metrics
	syncWorker                    *db.SyncWorker
	internalState                 *common.InternalState
	fiatRates                     *fiat.FiatRates
	callbacksOnNewBlock           []bchain.OnNewBlockFunc
	callbacksOnNewTxAddr          []bchain.OnNewTxAddrFunc
	callbacksOnNewTx              []bchain.OnNewTxFunc
	callbacksOnNewFiatRatesTicker []fiat.OnNewFiatRatesTicker
	chanOsSignal                  chan os.Signal
)

func init() {
	glog.MaxSize = 1024 * 1024 * 8
	glog.CopyStandardLogTo("INFO")
}
func main() {
	// 使用defer和recover来捕获和处理main函数或它调用的任何函数中发生的panic。
	// 记录panic的信息和调用栈，以便后续分析和调试。
	// 确保即使在遇到panic的情况下，程序也能通过调用os.Exit以指定的退出码安全退出，这里选择的是-1作为错误的标识。

	// note：defer和recover的使用是为了保证程序在遇到panic的情况下能够安全退出，而不是为了处理错误。
	// 一般情况下，我们应该尽量避免使用panic和recover，而是使用error来处理错误。
	// 但是在某些情况下，比如程序遇到了不可恢复的错误，或者是程序的状态已经不再可控，这时候使用panic和recover是合理的。

	// panic捕获要求defer和recover必须成对出现，defer用于注册一个函数，这个函数会在当前函数退出时被调用，recover用于捕获panic。
	// 但是，defer和recover并不是一对一的关系，defer可以注册多个函数，recover只能捕获最近的一个panic。

	// 一般情况下，我们会在main函数中使用defer和recover来捕获和处理main函数或它调用的任何函数中发生的panic。
	// 但是，如果main函数中的defer和recover不能捕获到panic，那么程序就会直接退出，而不会执行defer中注册的函数。
	// 所以，为了确保程序能够安全退出，我们可以在main函数中使用defer和recover来捕获和处理main函数或它调用的任何函数中发生的panic。

	// 捕获逻辑和panic出现的位置必须在同一个goroutine中，否则无法捕获panic。
	defer func() {
		if e := recover(); e != nil {
			glog.Error("main recovered from panic: ", e)
			debug.PrintStack() // 记录panic的信息和调用栈，以便后续分析和调试。
			os.Exit(-1)        // 正常退出的程序会返回0，非0的返回码通常用于指示错误。
		}
	}()
	os.Exit(mainWithExitCode())
}

// allow deferred functions to run even in case of fatal error
func mainWithExitCode() int {
	flag.Parse()

	// glog.Flush()会将日志缓冲区的日志写入到文件中，然后关闭文件。
	defer glog.Flush()

	// rand.Seed()用于设置随机数生成器的种子，不同的种子会产生不同的随机数序列。
	rand.Seed(time.Now().UTC().UnixNano())

	// make(chan os.Signal, 1)创建一个容量为1的通道，用于接收操作系统的信号。
	chanOsSignal = make(chan os.Signal, 1)
	// signal.Notify()用于将指定的信号发送到指定的通道, 通常用于监听操作系统的信号。
	// syscall.SIGHUP：终端挂起或控制进程终止, 通常是终端关闭或者用户注销。
	// syscall.SIGINT：键盘中断（如break键被按下）, 通常是Ctrl+C或Ctrl+D。
	// syscall.SIGQUIT：键盘的退出键被按下, 通常是Ctrl+\或Ctrl+4。
	// syscall.SIGTERM：进程终止信号, 通常用来要求程序自己正常退出。
	signal.Notify(chanOsSignal, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	// %+v 用于 common.GetVersionInfo() 的返回值，%+v 的作用是在打印结构体时会包括字段名，这对于打印更详细的信息很有帮助。
	// %v 用于 *debugMode 的值，%v 的作用是打印变量的默认格式。
	glog.Infof("Blockbook: %+v, debug mode %v", common.GetVersionInfo(), *debugMode)

	// 如果 *prof 不为空，则启动一个 http server，用于监听指定的地址和端口，并提供性能分析数据。
	if *prof != "" {
		go func() {
			log.Println(http.ListenAndServe(*prof, nil))
		}()
	}

	// 修复database
	if *repair {
		if err := db.RepairRocksDB(*dbPath); err != nil {
			glog.Errorf("RepairRocksDB %s: %v", *dbPath, err)
			return exitCodeFatal
		}
		return exitCodeOK
	}

	// 区块链RPC服务配置json文件的路径
	config, err := common.GetConfig(*configFile)
	if err != nil {
		glog.Error("config: ", err)
		return exitCodeFatal
	}

	// 获取prometheus监控指标，如果获取失败，则记录错误并返回
	metrics, err = common.GetMetrics(config.CoinName)
	if err != nil {
		glog.Error("metrics: ", err)
		return exitCodeFatal
	}

	if chain, mempool, err = getBlockChainWithRetry(config.CoinName, *configFile, pushSynchronizationHandler, metrics, 120); err != nil {
		glog.Error("rpc: ", err)
		return exitCodeFatal
	}

	index, err = db.NewRocksDB(*dbPath, *dbCache, *dbMaxOpenFiles, chain.GetChainParser(), metrics, *extendedIndex)
	if err != nil {
		glog.Error("rocksDB: ", err)
		return exitCodeFatal
	}
	defer index.Close()

	internalState, err = newInternalState(config, index, *enableSubNewTx)
	if err != nil {
		glog.Error("internalState: ", err)
		return exitCodeFatal
	}

	// fix possible inconsistencies in the UTXO index
	if *fixUtxo || !internalState.UtxoChecked {
		err = index.FixUtxos(chanOsSignal)
		if err != nil {
			glog.Error("fixUtxos: ", err)
			return exitCodeFatal
		}
		internalState.UtxoChecked = true
	}

	// sort addressContracts if necessary
	if !internalState.SortedAddressContracts {
		err = index.SortAddressContracts(chanOsSignal)
		if err != nil {
			glog.Error("sortAddressContracts: ", err)
			return exitCodeFatal
		}
		internalState.SortedAddressContracts = true
	}

	index.SetInternalState(internalState)
	if *fixUtxo {
		err = index.StoreInternalState(internalState)
		if err != nil {
			glog.Error("StoreInternalState: ", err)
			return exitCodeFatal
		}
		return exitCodeOK
	}

	if internalState.DbState != common.DbStateClosed {
		if internalState.DbState == common.DbStateInconsistent {
			glog.Error("internalState: database is in inconsistent state and cannot be used")
			return exitCodeFatal
		}
		glog.Warning("internalState: database was left in open state, possibly previous ungraceful shutdown")
	}

	if *computeFeeStatsFlag {
		internalState.DbState = common.DbStateOpen
		err = computeFeeStats(chanOsSignal, *blockFrom, *blockUntil, index, chain, txCache, internalState, metrics)
		if err != nil && err != db.ErrOperationInterrupted {
			glog.Error("computeFeeStats: ", err)
			return exitCodeFatal
		}
		return exitCodeOK
	}

	if *computeColumnStats {
		internalState.DbState = common.DbStateOpen
		err = index.ComputeInternalStateColumnStats(chanOsSignal)
		if err != nil {
			glog.Error("internalState: ", err)
			return exitCodeFatal
		}
		glog.Info("DB size on disk: ", index.DatabaseSizeOnDisk(), ", DB size as computed: ", internalState.DBSizeTotal())
		return exitCodeOK
	}

	syncWorker, err = db.NewSyncWorker(index, chain, *syncWorkers, *syncChunk, *blockFrom, *dryRun, chanOsSignal, metrics, internalState)
	if err != nil {
		glog.Errorf("NewSyncWorker %v", err)
		return exitCodeFatal
	}

	// set the DbState to open at this moment, after all important workers are initialized
	internalState.DbState = common.DbStateOpen
	err = index.StoreInternalState(internalState)
	if err != nil {
		glog.Error("internalState: ", err)
		return exitCodeFatal
	}

	if *rollbackHeight >= 0 {
		err = performRollback()
		if err != nil {
			return exitCodeFatal
		}
		return exitCodeOK
	}

	if txCache, err = db.NewTxCache(index, chain, metrics, internalState, !*noTxCache); err != nil {
		glog.Error("txCache ", err)
		return exitCodeFatal
	}

	if fiatRates, err = fiat.NewFiatRates(index, config, metrics, onNewFiatRatesTicker); err != nil {
		glog.Error("fiatRates ", err)
		return exitCodeFatal
	}

	// report BlockbookAppInfo metric, only log possible error
	if err = blockbookAppInfoMetric(index, chain, txCache, internalState, metrics); err != nil {
		glog.Error("blockbookAppInfoMetric ", err)
	}

	var internalServer *server.InternalServer
	if *internalBinding != "" {
		internalServer, err = startInternalServer()
		if err != nil {
			glog.Error("internal server: ", err)
			return exitCodeFatal
		}
	}

	var publicServer *server.PublicServer
	if *publicBinding != "" {
		publicServer, err = startPublicServer()
		if err != nil {
			glog.Error("public server: ", err)
			return exitCodeFatal
		}
	}

	if *synchronize {
		internalState.SyncMode = true
		internalState.InitialSync = true
		if err := syncWorker.ResyncIndex(nil, true); err != nil {
			if err != db.ErrOperationInterrupted {
				glog.Error("resyncIndex ", err)
				return exitCodeFatal
			}
			return exitCodeOK
		}
		// initialize mempool after the initial sync is complete
		var addrDescForOutpoint bchain.AddrDescForOutpointFunc
		if chain.GetChainParser().GetChainType() == bchain.ChainBitcoinType {
			addrDescForOutpoint = index.AddrDescForOutpoint
		}
		err = chain.InitializeMempool(addrDescForOutpoint, onNewTxAddr, onNewTx)
		if err != nil {
			glog.Error("initializeMempool ", err)
			return exitCodeFatal
		}
		var mempoolCount int
		if mempoolCount, err = mempool.Resync(); err != nil {
			glog.Error("resyncMempool ", err)
			return exitCodeFatal
		}
		internalState.FinishedMempoolSync(mempoolCount)
		go syncIndexLoop()
		go syncMempoolLoop()
		internalState.InitialSync = false
	}
	go storeInternalStateLoop()

	if publicServer != nil {
		// start full public interface
		callbacksOnNewBlock = append(callbacksOnNewBlock, publicServer.OnNewBlock)
		callbacksOnNewTxAddr = append(callbacksOnNewTxAddr, publicServer.OnNewTxAddr)
		callbacksOnNewTx = append(callbacksOnNewTx, publicServer.OnNewTx)
		callbacksOnNewFiatRatesTicker = append(callbacksOnNewFiatRatesTicker, publicServer.OnNewFiatRatesTicker)
		publicServer.ConnectFullPublicInterface()
	}

	if *blockFrom >= 0 {
		if *blockUntil < 0 {
			*blockUntil = *blockFrom
		}
		height := uint32(*blockFrom)
		until := uint32(*blockUntil)

		if !*synchronize {
			if err = syncWorker.ConnectBlocksParallel(height, until); err != nil {
				if err != db.ErrOperationInterrupted {
					glog.Error("connectBlocksParallel ", err)
					return exitCodeFatal
				}
				return exitCodeOK
			}
		}
	}

	if internalServer != nil || publicServer != nil || chain != nil {
		// start fiat rates downloader only if not shutting down immediately
		initDownloaders(index, chain, config)
		waitForSignalAndShutdown(internalServer, publicServer, chain, 10*time.Second)
	}

	if *synchronize {
		close(chanSyncIndex)
		close(chanSyncMempool)
		close(chanStoreInternalState)
		<-chanSyncIndexDone
		<-chanSyncMempoolDone
		<-chanStoreInternalStateDone
	}
	return exitCodeOK
}

// getBlockChainWithRetry
// 主要目的是尝试创建和初始化一个区块链接口及其内存池（mempool），并且具备重试机制。如果在第一次尝试时遇到错误，它会根据提供的秒数重试，直到成功或超出重试次数。这个函数对于处理网络延迟或暂时性服务不可用情况很有用，尤其是在初始化区块链连接时。下面是代码的详细解释：
// 函数参数：
// coin: 代表加密货币的字符串，如"BTC"、"ETH"等。
// configFile: 区块链节点配置文件的路径。
// pushHandler: 一个回调函数，用于处理区块链事件通知。
// metrics: 指向一个common.Metrics结构体的指针，用于收集和监控指标数据。
// seconds: 重试的最大秒数，也可以理解为重试的最大次数。
// 内部变量：
// chain: 用于存储初始化成功后的区块链接口。
// mempool: 存储初始化成功后的内存池接口。
// err: 存储错误信息。
// 重试逻辑：
// 使用time.NewTimer创建一个计时器，每次重试间隔1秒。
// 通过无限循环，使用coins.NewBlockChain函数尝试创建和初始化区块链和内存池。
// 如果coins.NewBlockChain返回错误，并且当前重试次数小于seconds参数，则记录错误信息，并等待计时器超时后再次重试。
// 如果在chanOsSignal通道接收到信号（这可能是一个用于处理系统中断信号的通道），则提前退出并返回错误，表示初始化过程被中断。
// 如果超过了最大重试次数还未成功，则返回错误。
// 成功返回：
// 如果coins.NewBlockChain成功返回，没有错误，那么函数将返回初始化好的chain和mempool接口，以及nil错误。
// 这个函数的设计考虑到了健壮性和灵活性，能够在遇到暂时性的连接问题时通过重试机制提高成功率，同时也提供了中断机制以响应系统退出信号。
func getBlockChainWithRetry(coin string, configFile string, pushHandler func(bchain.NotificationType), metrics *common.Metrics, seconds int) (bchain.BlockChain, bchain.Mempool, error) {
	var chain bchain.BlockChain
	var mempool bchain.Mempool
	var err error
	timer := time.NewTimer(time.Second)
	for i := 0; ; i++ {
		if chain, mempool, err = coins.NewBlockChain(coin, configFile, pushHandler, metrics); err != nil {
			if i < seconds {
				glog.Error("rpc: ", err, " Retrying...")
				select {
				case <-chanOsSignal:
					return nil, nil, errors.New("Interrupted")
				case <-timer.C:
					timer.Reset(time.Second)
					continue
				}
			} else {
				return nil, nil, err
			}
		}
		return chain, mempool, nil
	}
}

func startInternalServer() (*server.InternalServer, error) {
	internalServer, err := server.NewInternalServer(*internalBinding, *certFiles, index, chain, mempool, txCache, metrics, internalState, fiatRates)
	if err != nil {
		return nil, err
	}
	go func() {
		err = internalServer.Run()
		if err != nil {
			if err.Error() == "http: Server closed" {
				glog.Info("internal server: closed")
			} else {
				glog.Error(err)
				return
			}
		}
	}()
	return internalServer, nil
}

func startPublicServer() (*server.PublicServer, error) {
	// start public server in limited functionality, extend it after sync is finished by calling ConnectFullPublicInterface
	publicServer, err := server.NewPublicServer(*publicBinding, *certFiles, index, chain, mempool, txCache, *explorerURL, metrics, internalState, fiatRates, *debugMode)
	if err != nil {
		return nil, err
	}
	go func() {
		err = publicServer.Run()
		if err != nil {
			if err.Error() == "http: Server closed" {
				glog.Info("public server: closed")
			} else {
				glog.Error(err)
				return
			}
		}
	}()
	return publicServer, err
}

func performRollback() error {
	bestHeight, bestHash, err := index.GetBestBlock()
	if err != nil {
		glog.Error("rollbackHeight: ", err)
		return err
	}
	if uint32(*rollbackHeight) > bestHeight {
		glog.Infof("nothing to rollback, rollbackHeight %d, bestHeight: %d", *rollbackHeight, bestHeight)
	} else {
		hashes := []string{bestHash}
		for height := bestHeight - 1; height >= uint32(*rollbackHeight); height-- {
			hash, err := index.GetBlockHash(height)
			if err != nil {
				glog.Error("rollbackHeight: ", err)
				return err
			}
			hashes = append(hashes, hash)
		}
		err = syncWorker.DisconnectBlocks(uint32(*rollbackHeight), bestHeight, hashes)
		if err != nil {
			glog.Error("rollbackHeight: ", err)
			return err
		}
	}
	return nil
}

func blockbookAppInfoMetric(db *db.RocksDB, chain bchain.BlockChain, txCache *db.TxCache, is *common.InternalState, metrics *common.Metrics) error {
	api, err := api.NewWorker(db, chain, mempool, txCache, metrics, is, fiatRates)
	if err != nil {
		return err
	}
	si, err := api.GetSystemInfo(false)
	if err != nil {
		return err
	}
	subversion := si.Backend.Subversion
	if subversion == "" {
		// for coins without subversion (ETH) use ConsensusVersion as subversion in metrics
		subversion = si.Backend.ConsensusVersion
	}

	metrics.BlockbookAppInfo.Reset()
	metrics.BlockbookAppInfo.With(common.Labels{
		"blockbook_version":        si.Blockbook.Version,
		"blockbook_commit":         si.Blockbook.GitCommit,
		"blockbook_buildtime":      si.Blockbook.BuildTime,
		"backend_version":          si.Backend.Version,
		"backend_subversion":       subversion,
		"backend_protocol_version": si.Backend.ProtocolVersion}).Set(float64(0))
	metrics.BackendBestHeight.Set(float64(si.Backend.Blocks))
	metrics.BlockbookBestHeight.Set(float64(si.Blockbook.BestHeight))
	return nil
}

func newInternalState(config *common.Config, d *db.RocksDB, enableSubNewTx bool) (*common.InternalState, error) {
	is, err := d.LoadInternalState(config)
	if err != nil {
		return nil, err
	}

	is.EnableSubNewTx = enableSubNewTx
	name, err := os.Hostname()
	if err != nil {
		glog.Error("get hostname ", err)
	} else {
		if i := strings.IndexByte(name, '.'); i > 0 {
			name = name[:i]
		}
		is.Host = name
	}

	is.WsGetAccountInfoLimit, _ = strconv.Atoi(os.Getenv(strings.ToUpper(is.CoinShortcut) + "_WS_GETACCOUNTINFO_LIMIT"))
	if is.WsGetAccountInfoLimit > 0 {
		glog.Info("WsGetAccountInfoLimit enabled with limit ", is.WsGetAccountInfoLimit)
		is.WsLimitExceedingIPs = make(map[string]int)
	}
	return is, nil
}

func syncIndexLoop() {
	defer close(chanSyncIndexDone)
	glog.Info("syncIndexLoop starting")
	// resync index about every 15 minutes if there are no chanSyncIndex requests, with debounce 1 second
	common.TickAndDebounce(time.Duration(*resyncIndexPeriodMs)*time.Millisecond, debounceResyncIndexMs*time.Millisecond, chanSyncIndex, func() {
		if err := syncWorker.ResyncIndex(onNewBlockHash, false); err != nil {
			glog.Error("syncIndexLoop ", errors.ErrorStack(err), ", will retry...")
			// retry once in case of random network error, after a slight delay
			time.Sleep(time.Millisecond * 2500)
			if err := syncWorker.ResyncIndex(onNewBlockHash, false); err != nil {
				glog.Error("syncIndexLoop ", errors.ErrorStack(err))
			}
		}
	})
	glog.Info("syncIndexLoop stopped")
}

func onNewBlockHash(hash string, height uint32) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("onNewBlockHash recovered from panic: ", r)
		}
	}()
	for _, c := range callbacksOnNewBlock {
		c(hash, height)
	}
}

func onNewFiatRatesTicker(ticker *common.CurrencyRatesTicker) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("onNewFiatRatesTicker recovered from panic: ", r)
			debug.PrintStack()
		}
	}()
	for _, c := range callbacksOnNewFiatRatesTicker {
		c(ticker)
	}
}

func syncMempoolLoop() {
	defer close(chanSyncMempoolDone)
	glog.Info("syncMempoolLoop starting")
	// resync mempool about every minute if there are no chanSyncMempool requests, with debounce 1 second
	common.TickAndDebounce(time.Duration(*resyncMempoolPeriodMs)*time.Millisecond, debounceResyncMempoolMs*time.Millisecond, chanSyncMempool, func() {
		internalState.StartedMempoolSync()
		if count, err := mempool.Resync(); err != nil {
			glog.Error("syncMempoolLoop ", errors.ErrorStack(err))
		} else {
			internalState.FinishedMempoolSync(count)

		}
	})
	glog.Info("syncMempoolLoop stopped")
}

func storeInternalStateLoop() {
	stopCompute := make(chan os.Signal)
	defer func() {
		close(stopCompute)
		close(chanStoreInternalStateDone)
	}()
	signal.Notify(stopCompute, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	var computeRunning bool
	lastCompute := time.Now()
	lastAppInfo := time.Now()
	logAppInfoPeriod := 15 * time.Minute
	// randomize the duration between ComputeInternalStateColumnStats to avoid peaks after reboot of machine with multiple blockbooks
	computePeriod := time.Duration(*dbStatsPeriodHours)*time.Hour + time.Duration(rand.Float64()*float64((4*time.Hour).Nanoseconds()))
	if (*dbStatsPeriodHours) > 0 {
		glog.Info("storeInternalStateLoop starting with db stats recompute period ", computePeriod)
	} else {
		glog.Info("storeInternalStateLoop starting with db stats compute disabled")
	}
	common.TickAndDebounce(storeInternalStatePeriodMs*time.Millisecond, (storeInternalStatePeriodMs-1)*time.Millisecond, chanStoreInternalState, func() {
		if (*dbStatsPeriodHours) > 0 && !computeRunning && lastCompute.Add(computePeriod).Before(time.Now()) {
			computeRunning = true
			go func() {
				err := index.ComputeInternalStateColumnStats(stopCompute)
				if err != nil {
					glog.Error("computeInternalStateColumnStats error: ", err)
				}
				lastCompute = time.Now()
				computeRunning = false
			}()
		}
		if err := index.StoreInternalState(internalState); err != nil {
			glog.Error("storeInternalStateLoop ", errors.ErrorStack(err))
		}
		if lastAppInfo.Add(logAppInfoPeriod).Before(time.Now()) {
			if glog.V(1) {
				glog.Info(index.GetMemoryStats())
			}
			if err := blockbookAppInfoMetric(index, chain, txCache, internalState, metrics); err != nil {
				glog.Error("blockbookAppInfoMetric ", err)
			}
			lastAppInfo = time.Now()
		}
	})
	glog.Info("storeInternalStateLoop stopped")
}

func onNewTxAddr(tx *bchain.Tx, desc bchain.AddressDescriptor) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("onNewTxAddr recovered from panic: ", r)
		}
	}()
	for _, c := range callbacksOnNewTxAddr {
		c(tx, desc)
	}
}

func onNewTx(tx *bchain.MempoolTx) {
	defer func() {
		if r := recover(); r != nil {
			glog.Error("onNewTx recovered from panic: ", r)
		}
	}()
	for _, c := range callbacksOnNewTx {
		c(tx)
	}
}

func pushSynchronizationHandler(nt bchain.NotificationType) {
	glog.V(1).Info("MQ: notification ", nt)
	if common.IsInShutdown() {
		return
	}
	if nt == bchain.NotificationNewBlock {
		chanSyncIndex <- struct{}{}
	} else if nt == bchain.NotificationNewTx {
		chanSyncMempool <- struct{}{}
	} else {
		glog.Error("MQ: unknown notification sent")
	}
}

func waitForSignalAndShutdown(internal *server.InternalServer, public *server.PublicServer, chain bchain.BlockChain, timeout time.Duration) {
	sig := <-chanOsSignal
	common.SetInShutdown()
	glog.Infof("shutdown: %v", sig)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if internal != nil {
		if err := internal.Shutdown(ctx); err != nil {
			glog.Error("internal server: shutdown error: ", err)
		}
	}

	if public != nil {
		if err := public.Shutdown(ctx); err != nil {
			glog.Error("public server: shutdown error: ", err)
		}
	}

	if chain != nil {
		if err := chain.Shutdown(ctx); err != nil {
			glog.Error("rpc: shutdown error: ", err)
		}
	}
}

// computeFeeStats computes fee distribution in defined blocks
func computeFeeStats(stopCompute chan os.Signal, blockFrom, blockTo int, db *db.RocksDB, chain bchain.BlockChain, txCache *db.TxCache, is *common.InternalState, metrics *common.Metrics) error {
	start := time.Now()
	glog.Info("computeFeeStats start")
	api, err := api.NewWorker(db, chain, mempool, txCache, metrics, is, fiatRates)
	if err != nil {
		return err
	}
	err = api.ComputeFeeStats(blockFrom, blockTo, stopCompute)
	glog.Info("computeFeeStats finished in ", time.Since(start))
	return err
}

func initDownloaders(db *db.RocksDB, chain bchain.BlockChain, config *common.Config) {
	if fiatRates.Enabled {
		go fiatRates.RunDownloader()
	}
	if config.FourByteSignatures != "" && chain.GetChainParser().GetChainType() == bchain.ChainEthereumType {
		fbsd, err := fourbyte.NewFourByteSignaturesDownloader(db, config.FourByteSignatures)
		if err != nil {
			glog.Errorf("NewFourByteSignaturesDownloader Init error: %v", err)
		} else {
			glog.Infof("Starting FourByteSignatures downloader...")
			go fbsd.Run()
		}
	}
}
