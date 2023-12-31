package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	pb "github.com/arcward/keyquarry/api"
	kc "github.com/arcward/keyquarry/client"
	"github.com/arcward/keyquarry/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var signals = make(chan os.Signal, 1)
var ctx context.Context
var cancel context.CancelFunc
var defaultClientID = "foo"
var privilegedClientID = "someprivilegedidhere"
var testTimeout = 60 * time.Second
var dialTimeout = 10 * time.Second

type AddrTest struct {
	ListenAddress string
	SocketFile    string
	URL           *url.URL
}

func newClient(t *testing.T, cfg *kc.Config, addr AddrTest) *kc.Client {
	t.Helper()

	if cfg == nil {
		cfg = &kc.Config{
			NoTLS:    true,
			Address:  addr.ListenAddress,
			ClientID: defaultClientID,
		}
	}
	if cfg.Address == "" {
		cfg.Address = addr.ListenAddress
	}

	client := kc.NewClient(
		cfg,
		grpc.WithBlock(),
	)
	connCtx, connCancel := context.WithTimeout(ctx, dialTimeout)
	err := client.Dial(connCtx, true)

	if err != nil {
		panic(err)
	}
	connCancel()
	t.Cleanup(
		func() {
			discErr := client.CloseConnection()
			if discErr != nil {
				panic(discErr)
			}
		},
	)
	return client
}

func socketAddr(t *testing.T) AddrTest {
	t.Helper()
	tdir := t.TempDir()
	unixSocket := filepath.Join(tdir, fmt.Sprintf("%s.sock", t.Name()))
	listenAddress := fmt.Sprintf("unix://%s", unixSocket)
	addr := AddrTest{
		ListenAddress: listenAddress,
		SocketFile:    unixSocket,
	}
	u, err := parseURL(listenAddress)
	if err != nil {
		t.Fatalf("error parsing listen address: %s", err.Error())
	}
	addr.URL = u
	return addr

}

func newServer(
	t *testing.T,
	cfg *server.Config,
	addr AddrTest,
) *server.KeyValueStore {
	t.Helper()
	opts := &cliOpts
	newOpts := &CLIConfig{
		ServerOpts: *server.DefaultConfig(),
		ClientOpts: kc.DefaultConfig(),
	}
	*opts = *newOpts

	var srv *server.KeyValueStore
	var gServer *grpc.Server

	//log.SetOutput(io.Discard)

	td := os.Getenv("TEST_TIMEOUT")
	if td != "" {
		var ee error
		testTimeout, ee = time.ParseDuration(td)
		if ee != nil {
			panic(
				fmt.Sprintf(
					"failed to parse TEST_TIMEOUT: %s",
					ee.Error(),
				),
			)
		}
	}

	tctx, tcancel := context.WithTimeout(ctx, testTimeout)
	go func() {
		select {
		case <-signals:
			panic(fmt.Sprintf("%s: interrupted", t.Name()))
		case <-tctx.Done():
			if e := tctx.Err(); errors.Is(e, context.DeadlineExceeded) {
				t.Fatalf("%s: timeout exceeded", t.Name())
			}
		}
	}()

	t.Cleanup(
		func() {
			tcancel()
			rootCmd.SetContext(ctx)
		},
	)
	rootCmd.SetContext(tctx)

	var err error
	if cfg == nil {
		cfg = server.DefaultConfig()

		cfg.HashAlgorithm = crypto.MD5
		cfg.RevisionLimit = 2
		cfg.MinLifespan = time.Duration(1) * time.Second
		cfg.MinLockDuration = time.Duration(1) * time.Second
		cfg.EagerPrune = false
		cfg.PruneInterval = 0
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
		cfg.ListenAddress = addr.URL.String()
		slog.SetDefault(cfg.Logger)
		srv, err = server.NewServer(cfg)
		if err != nil {
			panic(err)
		}
	} else {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
		cfg.ListenAddress = addr.URL.String()
		slog.SetDefault(cfg.Logger)
		srv, err = server.NewServer(cfg)
		if err != nil {
			panic(err)
		}
	}

	fatalOnErr(
		t,
		clientCmd.PersistentFlags().Set("address", addr.ListenAddress),
	)

	blis, err := net.Listen(addr.URL.Scheme, addr.URL.Host)
	if err != nil {
		panic(err)
	}

	if gServer == nil {
		gServer = grpc.NewServer(
			grpc.UnaryInterceptor(server.ClientIDInterceptor(srv)),
			grpc.KeepaliveEnforcementPolicy(
				keepalive.EnforcementPolicy{
					MinTime:             5 * time.Second,
					PermitWithoutStream: true,
				},
			),
		)
	}

	pb.RegisterKeyValueStoreServer(gServer, srv)

	srvDone := make(chan struct{})
	go func() {
		defer func() {
			srvDone <- struct{}{}
		}()
		if e := srv.Start(tctx); e != nil {
			panic(e)
		}
	}()
	go func() {
		if se := gServer.Serve(blis); se != nil {
			panic(se)
		}
	}()

	t.Cleanup(
		func() {
			t.Logf("cancelling")
			gServer.GracefulStop()
			tcancel()
			<-srvDone
			gServer.Stop()
		},
	)

	socketCtx, socketCancel := context.WithTimeout(tctx, 15*time.Second)
	for {
		if socketCtx.Err() != nil {
			t.Fatalf(
				"error waiting for socket '%s': %s",
				srv.Config().ListenAddress,
				socketCtx.Err().Error(),
			)
		}
		_, err = os.Stat(addr.SocketFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err: %s", err.Error())
		} else if err == nil {
			t.Logf("no error statting %s", addr.SocketFile)
			socketCancel()
			break
		}
		time.Sleep(1 * time.Second)
	}
	return srv
}

func clientCtx(t *testing.T) context.Context {
	t.Helper()
	md := metadata.New(map[string]string{"client_id": defaultClientID})
	ictx := metadata.NewIncomingContext(ctx, md)

	return ictx
}

func captureOutput(t *testing.T, f func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("error with pipe: %s", err.Error())
	}
	t.Cleanup(
		func() {
			out = os.Stdout
		},
	)
	out = w

	outC := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, e := io.Copy(&buf, r)
		if e != nil {
			panic(err)
		}
		outC <- buf.String()
	}()
	f()
	err = w.Close()
	if err != nil {
		panic(err)
	}
	o := <-outC
	d := strings.TrimSpace(o)
	t.Logf("result: %s", d)
	return d
}

func init() {
	td := os.Getenv("TEST_TIMEOUT")
	if td != "" {
		var err error
		testTimeout, err = time.ParseDuration(td)
		if err != nil {
			panic(fmt.Sprintf("failed to parse TEST_TIMEOUT: %s", err.Error()))
		}
	}

	ctx, cancel = context.WithCancel(context.Background())

	signal.Notify(signals, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		select {
		case <-signals:
			cancel()
			panic("interrupted")
		case <-ctx.Done():
			panic("canceled")
		}
	}()

	fmt.Println("init done")
}

// failOnErr is a helper function that takes the result of a function that
// only has 1 return value (error), and fails the test if the error is not nil.
// It's intended to reduce boilerplate code in tests.
func failOnErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("[%s] error: %s", t.Name(), err.Error())
	}
}

func TestSetCmd(t *testing.T) {

	_ = newServer(t, nil, socketAddr(t))
	var data string
	rootCmd.SetArgs([]string{"client", "--verbose", "set", "foo", "bar"})

	f := func() {
		failOnErr(t, setCmd.Execute())
	}
	data = captureOutput(t, f)
	rv := pb.SetResponse{Success: true, IsNew: true}
	expected, err := json.Marshal(rv)
	failOnErr(t, err)
	if err != nil {
		assertEqual(t, data, string(expected))
	}
	t.Logf("expected:\n%s\ngot:\n%s", string(expected), data)

}

func TestGetCmd(t *testing.T) {

	addr := socketAddr(t)
	srv := newServer(t, nil, addr)
	cctx := clientCtx(t)

	client := newClient(t, nil, addr)
	// set the first value
	value := []byte("baz")

	kx, err := client.Set(cctx, &pb.KeyValue{Key: "foo", Value: value})
	fatalOnErr(t, err)
	t.Logf("created key: %+v", kx)

	// make sure it was set correctly
	rv, err := client.Get(cctx, &pb.Key{Key: "foo"})
	failOnErr(t, err)
	assertEqual(t, string(rv.Value), string(value))
	t.Logf("got value: %+v", rv)

	clientAddr := client.Config().Address
	t.Logf("client addr: %s", clientAddr)

	srvAddr := srv.Config().ListenAddress
	t.Logf("srv addr: %s", srvAddr)
	// get it via CLI
	rootCmd.SetArgs([]string{"client", "--verbose", "get", "foo"})
	fatalOnErr(
		t,
		clientCmd.PersistentFlags().Set("address", addr.ListenAddress),
	)
	_, err = os.Stat(addr.SocketFile)
	if err == nil {
		t.Logf("file %s exists", addr.SocketFile)
	} else {
		t.Fatalf("error with socket file %s: %s", addr.SocketFile, err.Error())
	}
	data := captureOutput(
		t, func() {
			fatalOnErr(t, getCmd.Execute())
		},
	)
	assertEqual(t, data, string(value))

	// set two new values, so the original should be saved as
	// revision 1, this should be revision 2, and the final value
	// should not be in the history, just be the current value
	targetValue := []byte("newbaz")
	_, err = client.Set(
		cctx,
		&pb.KeyValue{Key: "foo", Value: targetValue},
	)
	failOnErr(t, err)

	finalValue := []byte("asdf")
	_, err = client.Set(
		cctx,
		&pb.KeyValue{Key: "foo", Value: finalValue},
	)
	failOnErr(t, err)

	// version 2 should be the value set prior to the current value
	rootCmd.SetArgs([]string{"client", "get", "foo", "--revision", "2"})
	getCmd.SetContext(cctx)
	data = captureOutput(
		t, func() {
			failOnErr(t, getCmd.Execute())
		},
	)
	assertEqual(t, data, string(targetValue))

	// `--revision=0` should return the current version
	rootCmd.SetArgs([]string{"client", "get", "foo", "--revision", "0"})
	data = captureOutput(
		t, func() {
			fatalOnErr(t, getCmd.Execute())
		},
	)
	assertEqual(t, data, string(finalValue))
}

func TestServerCmd(t *testing.T) {
	// When we call `server --config=...`, it will set `CLIConfig.configFile`
	// and read the config from there. If we don't reset it after the test,
	// the next test will fail as the file will no longer exist

	opts := &cliOpts
	newOpts := &CLIConfig{ServerOpts: *server.DefaultConfig()}
	*opts = *newOpts

	addr := socketAddr(t)

	// Set our own context to control when the server stops
	tctx, tcancel := context.WithTimeout(ctx, testTimeout*5)
	go func() {
		select {
		case <-signals:
			panic(fmt.Sprintf("%s: interrupted", t.Name()))
		case <-tctx.Done():
			if e := tctx.Err(); errors.Is(e, context.DeadlineExceeded) {
				t.Fatalf("%s: timeout exceeded", t.Name())
			}
		}
	}()

	t.Cleanup(
		func() {
			rootCmd.SetContext(ctx)
		},
	)

	srvContext, srvCancel := context.WithTimeout(tctx, testTimeout)
	go func() {
		select {
		case <-srvContext.Done():
			if e := srvContext.Err(); errors.Is(e, context.DeadlineExceeded) {
				t.Fatalf("%s: timeout exceeded", t.Name())
			}
		}
	}()
	rootCmd.SetContext(srvContext)
	serverCmd.SetContext(srvContext)

	t.Cleanup(
		func() {

			tcancel()
			serverCmd.SetContext(ctx)
			rootCmd.SetContext(ctx)
		},
	)

	tdir := t.TempDir()
	tempSnapshots := filepath.Join(tdir, "snapshots")

	secretKey := generateRandomSecretKey(t)
	snapshotInterval := 2 * time.Second
	snapshotLimit := 3

	var newMaxKeySize uint64 = 500
	var newMaxValueSize uint64 = 1234
	var newMaxKeys uint64 = 9876
	var newHashAlgorithm = crypto.SHA256
	var newRevisionLimit int64 = 1234

	tempConfig := map[string]string{
		"SNAPSHOT.ENCRYPT":    "true",
		"SNAPSHOT.SECRET_KEY": secretKey,
		"HASH_ALGORITHM":      newHashAlgorithm.String(),
		"SNAPSHOT.DIR":        tempSnapshots,
		"SNAPSHOT.INTERVAL":   snapshotInterval.String(),
		"SNAPSHOT.LIMIT":      fmt.Sprintf("%d", snapshotLimit),
		"SNAPSHOT.ENABLED":    "true",
		"LISTEN_ADDRESS":      addr.ListenAddress,
		"MAX_KEY_SIZE":        fmt.Sprintf("%d", newMaxKeySize),
		"MAX_KEYS":            fmt.Sprintf("%d", newMaxKeys),
		"MAX_VALUE_SIZE":      fmt.Sprintf("%d", newMaxValueSize),
		"REVISION_LIMIT":      fmt.Sprintf("%d", newRevisionLimit),
		"PRUNE_INTERVAL":      "1s",
		"MIN_PRUNE_INTERVAL":  "1s",
		"LOG_LEVEL":           "DEBUG",
	}

	clientCfg := kc.DefaultConfig()
	clientCfg.NoTLS = true
	clientCfg.Address = addr.ListenAddress
	clientCfg.ClientID = defaultClientID
	tmpClient := kc.NewClient(
		&clientCfg,
		grpc.WithBlock(),
	)
	configFile := filepath.Join(tdir, "temp.env")
	f, err := os.OpenFile(configFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("error writing %s: %s", configFile, err.Error())
	}

	writer := bufio.NewWriter(f)
	for k, v := range tempConfig {
		_, err = writer.WriteString(fmt.Sprintf("%s=%s\n", k, v))
		if err != nil {
			t.Fatalf("error writing %s: %s", configFile, err.Error())
		}
	}
	err = writer.Flush()
	if err != nil {
		t.Fatalf("error writing %s: %s", configFile, err.Error())
	}
	fatalOnErr(t, f.Close())

	rootCmd.SetArgs(
		[]string{
			"server",
			"--log-level",
			"INFO",
			"--config",
			configFile,
		},
	)

	// Execute the command to start the server, track when it's done
	// so we know it's safe to check for the existence of the socket file
	execDone := make(chan struct{}, 1)
	go func() {
		e := serverCmd.Execute()
		fatalOnErr(t, e)
		execDone <- struct{}{}
	}()

	// Wait for the socket file to exist, max 5 seconds
	socketCtx, socketCancel := context.WithTimeout(tctx, 15*time.Second)

	for {
		if socketCtx.Err() != nil {
			t.Fatalf(
				"error waiting for socket '%s': %s",
				addr.SocketFile,
				socketCtx.Err().Error(),
			)
		}
		_, err = os.Stat(addr.SocketFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err: %s", err.Error())
		} else if err == nil {
			t.Logf("no error statting %s", addr.SocketFile)
			socketCancel()
			break
		}
		time.Sleep(1 * time.Second)
	}

	connCtx, connCancel := context.WithTimeout(tctx, dialTimeout)
	err = tmpClient.Dial(connCtx, false)
	connCancel()
	if err != nil {
		t.Fatalf("error dialing: %s", err.Error())
	}
	t.Cleanup(
		func() {
			_ = tmpClient.CloseConnection()
		},
	)

	type Filename string
	// Maps filenames to key names
	snapshotsSeen := map[Filename]string{}
	// Maps the iteration number to the filename
	snapshotsByNum := map[int]Filename{}

	// Loop up to the snapshot limit + 1, so we can verify that the oldest
	// snapshot is deleted when the limit is reached
	for i := 0; i < snapshotLimit+1; i++ {
		// Create a new key on each iteration, with a name reflecting
		// each iteration number
		kv := &pb.KeyValue{Key: fmt.Sprintf("foo-%d", i), Value: []byte("bar")}
		rv, e := tmpClient.Set(srvContext, kv)
		if e != nil {
			t.Fatalf("error setting key: %s", err.Error())
		}
		assertEqual(t, rv.IsNew, true)
		assertEqual(t, rv.Success, true)
		ssctx, sscancel := context.WithTimeout(srvContext, 30*time.Second)

		// Loop until we see the snapshot file for the current key, or until
		// we time out
		for {
			if ssctx.Err() != nil {
				sscancel()
				if errors.Is(ssctx.Err(), context.DeadlineExceeded) {
					t.Fatalf("timeout waiting for snapshot %d", i)
				} else {
					t.Logf(
						"breaking out of snapshot on loop %d: %s",
						i,
						ssctx.Err().Error(),
					)
					break
				}
			}

			snapshots, se := filepath.Glob(
				filepath.Join(
					tempSnapshots,
					"*.json.aes.gz",
				),
			)
			if se != nil {
				sscancel()
				t.Fatalf("error globbing snapshots: %s", se.Error())
			}
			if len(snapshots) == 0 {
				continue
			}

			// If there are snapshots and we (should have) exceeded the
			// snapshot limit, verify that the oldest snapshot was deleted
			if i > snapshotLimit {
				firstSnapshot := string(snapshotsByNum[0])
				_, ssErr := os.Stat(firstSnapshot)
				if ssErr == nil || !os.IsNotExist(ssErr) {
					t.Fatalf(
						"expected first snapshot to be deleted, still exists: %s",
						firstSnapshot,
					)
				}
				// Verify that the number of snapshots is equal to the
				// snapshot limit, in addition to the oldest snapshot
				// having been deleted
				if len(snapshots) > snapshotLimit {
					t.Fatalf(
						"expected %d snapshots, got %d",
						snapshotLimit,
						len(snapshots),
					)
				}
			}

			// We should find a new snapshot file, and it should have
			// the key for the current iteration
			for _, sf := range snapshots {
				if ssctx.Err() != nil {
					if errors.Is(ssctx.Err(), context.DeadlineExceeded) {
						t.Fatalf("timeout waiting for snapshot %d", i)
					} else {
						t.Logf("breaking out of snapshot on loop %d", i)
						break
					}
				}
				sfilename := Filename(sf)
				_, fileSeen := snapshotsSeen[sfilename]

				if !fileSeen {
					t.Logf(
						"snapshot after creating %s: %s",
						kv.Key,
						sfilename,
					)
					snapdata, serr := server.ReadSnapshot(sf, secretKey)
					fatalOnErr(t, serr)
					for _, snapKey := range snapdata.Keys {
						if snapKey.Key == kv.Key {
							// once we find it, track it and cancel the
							// current loop's context so we don't
							// time out and can move on
							sscancel()
							snapshotsSeen[sfilename] = kv.Key
							snapshotsByNum[i] = sfilename
							break
						}
					}
					_, foundKey := snapshotsByNum[i]
					if !foundKey {
						t.Fatalf(
							"unable to find key %s in snapshot %s",
							kv.Key,
							sfilename,
						)
					}
				}
			}
		}
	}

	if len(snapshotsSeen) != snapshotLimit+1 {
		t.Fatalf(
			"expected %d snapshots, got %d",
			snapshotLimit+1,
			len(snapshotsSeen),
		)
	}

	t.Logf("snapshots seen: %#v / %#v", snapshotsByNum, snapshotsSeen)
	time.Sleep(1 * time.Second)
	srvCancel()
	// Wait for the command to finish, then verify it deleted the socket
	// file before returning
	<-execDone

	// Validate the end result of the config
	currentServer := cliOpts.server
	cfg := currentServer.Config()
	assertEqual(t, cfg.Snapshot.SecretKey, secretKey)
	assertEqual(t, cfg.Snapshot.Dir, tempSnapshots)
	assertEqual(t, cfg.Snapshot.Encrypt, true)
	assertEqual(t, cfg.Snapshot.Interval, snapshotInterval)
	assertEqual(t, cfg.Snapshot.Limit, snapshotLimit)
	assertEqual(t, cfg.HashAlgorithm, newHashAlgorithm)
	assertEqual(t, cfg.MaxKeySize, newMaxKeySize)
	assertEqual(t, cfg.MaxValueSize, newMaxValueSize)
	assertEqual(t, cfg.RevisionLimit, newRevisionLimit)
	assertEqual(t, cfg.MaxNumberOfKeys, newMaxKeys)
	assertEqual(t, cfg.SSLCertfile, "")
	assertEqual(t, cfg.SSLKeyfile, "")
	assertEqual(t, cfg.LogLevel, "INFO")
	assertEqual(t, cfg.LogJSON, false)

	u := &url.URL{Scheme: "unix", Host: addr.SocketFile}
	assertEqual(t, cfg.ListenAddress, u.String())

	fileInfo, err := os.Stat(addr.SocketFile)
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf(
			"socket file '%s' still exists (%d): %#v",
			addr.SocketFile,
			fileInfo.Size(),
			err,
		)
	}

	secondSnapshot := string(snapshotsByNum[1])
	_, ssErr := os.Stat(secondSnapshot)
	if ssErr == nil || !os.IsNotExist(ssErr) {
		t.Fatalf(
			"expected second snapshot to be deleted, still exists: %s",
			secondSnapshot,
		)
	}
	snapshots, se := filepath.Glob(
		filepath.Join(
			tempSnapshots,
			"*.json.aes.gz",
		),
	)
	failOnErr(t, se)
	assertEqual(t, len(snapshots), snapshotLimit)

	var finalSnapshot string
	lastSnapshotsSeen := []string{}
	for _, sf := range snapshots {
		sfilename := Filename(sf)
		lastSnapshotsSeen = append(lastSnapshotsSeen, sf)
		_, ok := snapshotsSeen[sfilename]
		if !ok {
			finalSnapshot = sf
		}
	}
	if finalSnapshot == "" {
		t.Fatalf(
			"unable to find final snapshot (current files: %+v), seen: %+v",
			lastSnapshotsSeen,
			snapshotsSeen,
		)
	} else {
		snapshotData, e := server.ReadSnapshot(finalSnapshot, secretKey)
		if e != nil {
			t.Fatalf(
				"error reading snapshot '%s': %s",
				finalSnapshot,
				e.Error(),
			)
		}
		assertEqual(t, len(snapshotData.Keys), snapshotLimit+1)
	}

	pruneHistory := currentServer.PruneHistory()
	if pruneHistory == nil {
		t.Errorf("expected pruneHistory to be set")
	}
	t.Logf("prune history: %#v", pruneHistory)

}

func fatalOnErr(t *testing.T, err error, msg ...string) {
	t.Helper()
	if err != nil {
		if len(msg) > 0 {
			t.Fatalf(
				"expected no error, got: %s (%s)",
				err.Error(),
				strings.Join(msg, " / "),
			)
		} else {
			t.Fatalf("expected no error, got: %s", err.Error())
		}
	}
}

func generateRandomSecretKey(t *testing.T) string {
	t.Helper()
	secretKey := make([]byte, 32)
	_, err := rand.Read(secretKey)
	if err != nil {
		t.Fatalf("error generating random secret key: %s", err.Error())
	}
	hexKey := hex.EncodeToString(secretKey)
	copy(secretKey, hexKey)
	return string(secretKey)
}

func TestListKeysCmd(t *testing.T) {
	addr := socketAddr(t)
	_ = newServer(t, nil, addr)
	client := newClient(t, nil, addr)

	cctx := clientCtx(t)
	_, err := client.Set(cctx, &pb.KeyValue{Key: "foo", Value: nil})
	failOnErr(t, err)

	_, err = client.Set(cctx, &pb.KeyValue{Key: "bar", Value: nil})
	failOnErr(t, err)

	_, err = client.Set(cctx, &pb.KeyValue{Key: "baz", Value: nil})
	failOnErr(t, err)

	rootCmd.SetArgs([]string{"client", "--verbose", "list"})

	data := captureOutput(
		t, func() {
			failOnErr(t, listCmd.Execute())
		},
	)

	keys := strings.Split(data, "\n")
	assertSliceContains(t, keys, "foo", "bar", "baz")
	assertEqual(t, len(keys), 3)

	failOnErr(t, listCmd.Flags().Set("limit", "2"))

	data = captureOutput(
		t, func() {
			failOnErr(t, listCmd.Execute())
		},
	)
	keys = strings.Split(data, "\n")
	assertEqual(t, len(keys), 2)

	failOnErr(t, listCmd.Flags().Set("pattern", "ba*"))
	data = captureOutput(
		t, func() {
			failOnErr(t, listCmd.Execute())
		},
	)
	keys = strings.Split(data, "\n")
	assertEqual(t, len(keys), 2)
	assertSliceContains(t, keys, "bar", "baz")

}

func assertSliceContains[V comparable](t *testing.T, value []V, expected ...V) {
	t.Helper()
	found := map[V]bool{}
	if len(expected) == 0 {
		t.Fatalf("expected slice must not be empty")
	}
	for _, v := range value {
		for _, exp := range expected {
			if v == exp {
				found[exp] = true
				break
			}
		}
	}
	if len(found) != len(expected) {
		t.Errorf(
			"expected:\n%#v\n\nnot found in:\n%#v", expected, value,
		)
	}
}

func assertEqual[V comparable](t *testing.T, val V, expected V) {
	t.Helper()
	if val != expected {
		t.Errorf(
			"expected:\n%#v\n\ngot:\n%#v",
			expected,
			val,
		)
	}
}

func TestSetReadonlyCmd(t *testing.T) {
	addr := socketAddr(t)
	cfg := server.DefaultConfig()
	cfg.PrivilegedClientID = privilegedClientID

	srv := newServer(t, cfg, addr)

	assertEqual(t, srv.Config().Readonly, false)
	rootCmd.SetArgs(
		[]string{
			"client",
			"--client-id",
			privilegedClientID,
			"readonly",
			"on",
		},
	)
	setReadonlyCmd.SetContext(ctx)

	data := captureOutput(
		t, func() {
			failOnErr(t, setReadonlyCmd.Execute())
		},
	)
	rv := pb.ReadOnlyResponse{Success: true}
	expected, err := json.Marshal(rv)
	failOnErr(t, err)
	assertEqual(t, data, string(expected))
	assertEqual(t, srv.Config().Readonly, true)

	rootCmd.SetArgs([]string{"client", "readonly", "off"})
	setReadonlyCmd.SetContext(ctx)
	data = captureOutput(
		t, func() {
			fatalOnErr(t, setReadonlyCmd.Execute())
		},
	)
	rv = pb.ReadOnlyResponse{Success: true}
	expected, err = json.Marshal(rv)
	fatalOnErr(t, err)
	assertEqual(t, data, string(expected))
	assertEqual(t, srv.Config().Readonly, false)

}

func TestLockCmd(t *testing.T) {
	addr := socketAddr(t)
	_ = newServer(t, nil, addr)
	client := newClient(t, nil, addr)
	rootCmd.SetArgs([]string{"client", "lock", "foo", "10s"})
	cctx := clientCtx(t)
	value := []byte("baz")
	_, err := client.Set(cctx, &pb.KeyValue{Key: "foo", Value: value})
	failOnErr(t, err)

	kvInfo, err := client.GetKeyInfo(cctx, &pb.Key{Key: "foo"})
	fatalOnErr(t, err)
	assertEqual(t, kvInfo.Locked, false)

	f := func() {
		failOnErr(t, lockCmd.Execute())
	}
	data := captureOutput(t, f)

	var lockResponse pb.LockResponse
	err = json.Unmarshal([]byte(data), &lockResponse)
	failOnErr(t, err)
	assertEqual(t, lockResponse.Success, true)

}

func TestLockCmdCreateIfMissing(t *testing.T) {
	addr := socketAddr(t)
	_ = newServer(t, nil, addr)
	rootCmd.SetArgs(
		[]string{
			"client",
			"lock",
			"somerandomkey",
			"10s",
			"--create-if-missing",
		},
	)

	f := func() {
		failOnErr(t, lockCmd.Execute())
	}
	data := captureOutput(t, f)

	var lockResponse pb.LockResponse
	err := json.Unmarshal([]byte(data), &lockResponse)
	failOnErr(t, err)
	assertEqual(t, lockResponse.Success, true)

}

func TestUnlockCmd(t *testing.T) {

	addr := socketAddr(t)

	_ = newServer(t, nil, addr)

	client := newClient(t, nil, addr)
	cctx := clientCtx(t)
	value := []byte("baz")
	kvSet, err := client.Set(
		cctx,
		&pb.KeyValue{
			Key:          "foo",
			Value:        value,
			LockDuration: durationpb.New(1 * time.Hour),
		},
	)
	fatalOnErr(t, err)
	assertEqual(t, kvSet.Success, true)

	rootCmd.SetArgs(
		[]string{
			"client",
			"unlock",
			"foo",
			"--client-id",
			defaultClientID,
		},
	)

	kvInfo, err := client.GetKeyInfo(cctx, &pb.Key{Key: "foo"})
	fatalOnErr(t, err)
	assertEqual(t, kvInfo.Locked, true)

	f := func() {
		fatalOnErr(t, unlockCmd.Execute())
	}
	data := captureOutput(t, f)

	lockResponse := pb.UnlockResponse{Success: true}
	expected, err := json.Marshal(lockResponse)

	assertEqual(t, data, string(expected))
}

func TestDeleteCmd(t *testing.T) {

	addr := socketAddr(t)
	_ = newServer(t, nil, addr)
	client := newClient(t, nil, addr)
	rootCmd.SetArgs([]string{"client", "delete", "foo"})
	value := []byte("baz")
	_, err := client.Set(
		clientCtx(t),
		&pb.KeyValue{
			Key:   "foo",
			Value: value,
		},
	)
	failOnErr(t, err)

	f := func() {
		failOnErr(t, deleteCmd.Execute())
	}
	data := captureOutput(t, f)

	deleteResponse := pb.DeleteResponse{Deleted: true}
	expected, err := json.Marshal(deleteResponse)

	assertEqual(t, data, string(expected))
}

func TestGetKeyInfoCmd(t *testing.T) {
	addr := socketAddr(t)
	_ = newServer(t, nil, addr)
	client := newClient(t, nil, addr)
	infoCmd.SetContext(ctx)
	rootCmd.SetArgs([]string{"client", "info", "foo"})
	value := []byte("baz")
	cctx := clientCtx(t)
	_, err := client.Set(
		cctx,
		&pb.KeyValue{
			Key:          "foo",
			Value:        value,
			LockDuration: durationpb.New(1 * time.Hour),
		},
	)
	failOnErr(t, err)

	f := func() {
		failOnErr(t, infoCmd.Execute())
	}
	data := captureOutput(t, f)

	kvInfo, err := client.GetKeyInfo(cctx, &pb.Key{Key: "foo"})
	failOnErr(t, err)
	expected, err := json.Marshal(kvInfo)
	failOnErr(t, err)

	assertEqual(t, data, string(expected))
}
