// Copyright 2020 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

// apitest is a cli tool (and Docker image) used for testing the Moov API.
// This tool is designed to operate against the production API and a local
// setup.
//
// With no arguments the contaier runs tests against the production API.
//
// apitest is not a stable tool. Please contact Moov developers if you intend to use this tool,
// otherwise we might change the tool (or remove it) without notice.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/moov-io/api"
	"github.com/moov-io/api/cmd/apitest/local"
	"github.com/moov-io/base"
	"github.com/moov-io/base/admin"
	"github.com/moov-io/base/http/bind"
	moov "github.com/moov-io/go-client/client"

	"github.com/go-kit/kit/metrics/prometheus"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"go4.org/syncutil"
)

var (
	defaultApiAddress = "https://api.moov.io"

	flagApiAddress = flag.String("address", defaultApiAddress, "Moov API address")
	flagDebug      = flag.Bool("debug", false, "Enable Debug logging.")
	flagLocal      = flag.Bool("local", false, "Use local HTTP addresses (e.g. 'go run')")
	flagLocalDev   = flag.Bool("dev", false, "Use tilt local HTTP address")

	flagPing    = flag.Bool("ping", false, "Ping Moov applications and quit")
	flagVersion = flag.Bool("version", false, "Show the version and quit")

	adminAddr = flag.String("admin.addr", bind.Admin("apitest"), "Admin HTTP listen address")

	// Business logic flags
	flagACHType = flag.String("ach.type", "PPD", "ACH Service Class Code (SEC) to use. Options: PPD, IAT")
	flagOAuth   = flag.Bool("oauth", false, "Use OAuth instead of cookie auth")

	flagCleanup = flag.Bool("cleanup", false, "Cleanup files, transfers, etc after creation")

	flagFakeData       = flag.Bool("fake-data", false, "Generate fake data (instead of one transfer) across several routing numbers, receivers, and originators")
	flagFakeIterations = flag.Int("fake-data.iterations", 1000, "How many users and transfers to create")

	flagCustomersAdminAddress = flag.String("customers.admin-address", fmt.Sprintf("http://localhost%s", bind.Admin("customers")), "HTTP address for Customers service")
	flagPaygateAdminAddress   = flag.String("paygate.admin-address", fmt.Sprintf("http://localhost%s", bind.Admin("paygate")), "HTTP address for Moov paygate service")

	// TODO(adam): can we run this in CI now? with paygate's docker-compose setup??
	flagVerifyTransfers    = flag.String("verify-transfers.dir", "", "Verify the created transfers exist in the given directory of ACH files")
	flagVerifyInitialSleep = flag.Duration("verify-transfers.initial-sleep", 1*time.Minute, "Duration to sleep so paygate can process and merge all transfers")

	flagPauseAfterTransfers = flag.Bool("pause", false, "time.Sleep after transfers (intended for prometheus to scrape metrics)")
	flagPauseDuration       = flag.Duration("pause.duration", 2*time.Minute, "Duration to pause for after transfers")
)

func main() {
	flag.Parse()

	if *flagVersion {
		fmt.Println(api.Version())
		return
	}

	log.SetFlags(log.Ldate | log.Ltime | log.LUTC | log.Lmicroseconds | log.Lshortfile)
	log.Printf("Starting apitest %s", api.Version())

	adminServer := admin.NewServer(*adminAddr)
	adminServer.AddVersionHandler(api.Version()) // Setup 'GET /version'
	go func() {
		log.Printf("listening on %s", adminServer.BindAddr())
		adminServer.Listen()
	}()
	defer adminServer.Shutdown()

	ctx := context.TODO()
	requestID := base.ID()

	// Basic sanity check against apps
	if err := pingApps(ctx, requestID); err != nil {
		log.Fatalf("FAILURE: %v", err)
	}
	if *flagPing {
		log.Println("INFO: all applications responded")
		return
	}

	// If we're going to verify we need the directory to be empty beforehand
	if *flagVerifyTransfers != "" && !verifyDirIsEmpty(*flagVerifyTransfers) {
		log.Fatalf("FAILURE: verify directory %s is not empty", *flagVerifyTransfers)
	}

	var mu sync.Mutex
	var iterations []*iteration

	// Run either one or many iterations
	if *flagFakeData {
		fmt.Println("") // add buffer space in output

		var wg sync.WaitGroup
		gate := syncutil.NewGate(10) // allow 10 concurrent iterations
		for i := 0; i < *flagFakeIterations; i++ {
			wg.Add(1)
			gate.Start()
			go func() {
				if iter := iterate(ctx, requestID); iter != nil {
					mu.Lock()
					iterations = append(iterations, iter)
					mu.Unlock()
				}
				gate.Done()
				wg.Done()
			}()
		}
		wg.Wait()
	} else {
		if iter := iterate(ctx, requestID); iter != nil {
			iterations = append(iterations, iter) // just one user and transfer

			// Verify you can't just add x-user-id
			ac := &authChecker{
				apiAddress: *flagApiAddress,
				requestID:  iter.requestID,
				userID:     iter.userID,

				origDepID:    iter.originatorDepository.ID,
				originatorID: iter.originator.ID,
				recDepID:     iter.receiverDepository.ID,
				receiverID:   iter.receiver.ID,
				transferID:   iter.transfer.ID,
			}
			if err := ac.checkAll(); err != nil {
				log.Fatalf("FAILURE: auth bypass %s", err)
			}
			log.Println("INFO: CORS headers present on all HTTP responses")
		}
	}

	// Verify every transfer we made exists
	if *flagVerifyTransfers != "" {
		if len(iterations) == 0 {
			log.Fatalf("FAILURE: unable to create any transfers, see above output logs for errors")
		}
		log.Printf("Sleeping for %v to let paygate collect and merge %d transfers", flagVerifyInitialSleep, len(iterations))
		time.Sleep(*flagVerifyInitialSleep)
		if err := verifyTransfersWereMerged(*flagVerifyTransfers, iterations); err != nil {
			log.Fatalf("FAILURE: %v", err)
		}
	}

	// Pause after transfers
	if *flagPauseAfterTransfers {
		log.Printf("pausing for %v\n", flagPauseDuration)
		time.Sleep(*flagPauseDuration)
	}
}

var apiAddressOnce sync.Once

func makeConfiguration() *moov.Configuration {
	conf := moov.NewConfiguration()
	if *flagLocal {
		// If '-local and -address <foo>' use <foo>
		if addr := *flagApiAddress; addr != defaultApiAddress {
			conf.BasePath = addr
		} else {
			conf.BasePath = "http://localhost"
		}
	} else {
		if *flagLocalDev {
			conf.BasePath = "http://localhost:9000"
		} else {
			conf.BasePath = *flagApiAddress
		}
	}
	if *flagDebug {
		conf.Debug = true
	}
	apiAddressOnce.Do(func() {
		log.Printf("Using %s as base API address", conf.BasePath)
	})
	conf.UserAgent = fmt.Sprintf("moov apitest/%s", api.Version())

	// setup HTTP client
	conf.HTTPClient = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			MaxConnsPerHost:     100,
			IdleConnTimeout:     1 * time.Minute,
		},
	}
	if *flagLocal {
		tr := conf.HTTPClient.Transport
		conf.HTTPClient.Transport = &local.Transport{
			Underlying: tr,
			Debug:      *flagDebug,
		}
	}
	return conf
}

func pingApps(ctx context.Context, requestID string) error {
	conf := makeConfiguration()
	conf.AddDefaultHeader("X-Request-ID", requestID)
	api := moov.NewAPIClient(conf)

	// ACH
	resp, err := api.MonitorApi.PingACH(ctx, &moov.PingACHOpts{})
	if err != nil {
		return fmt.Errorf("ERROR: failed to ping ACH: %v", err)
	}
	resp.Body.Close()
	log.Println("ACH PONG")

	// auth
	resp, err = api.MonitorApi.PingAuth(ctx, &moov.PingAuthOpts{})
	if err != nil {
		return fmt.Errorf("ERROR: failed to ping auth: %v", err)
	}
	resp.Body.Close()
	log.Println("auth PONG")

	// fed
	resp, err = api.MonitorApi.PingFED(ctx, &moov.PingFEDOpts{})
	if err != nil {
		return fmt.Errorf("ERROR: failed to ping FED: %v", err)
	}
	resp.Body.Close()
	log.Println("FED PONG")

	// Watchman
	resp, err = api.MonitorApi.PingWatchman(ctx, &moov.PingWatchmanOpts{})
	if err != nil {
		return fmt.Errorf("ERROR: failed to ping Watchman: %v", err)
	}
	resp.Body.Close()
	log.Println("Watchman PONG")

	// paygate
	resp, err = api.MonitorApi.PingPaygate(ctx, &moov.PingPaygateOpts{})
	if err != nil {
		return fmt.Errorf("ERROR: failed to ping paygate: %v", err)
	}
	resp.Body.Close()
	log.Println("paygate PONG")
	return nil
}

type iteration struct {
	user       *user
	oauthToken moov.OAuth2Token

	requestID string
	userID    string

	originator           moov.Originator
	originatorAccount    *moov.Account
	originatorDepository moov.Depository

	receiver           moov.Receiver
	receiverAccount    *moov.Account
	receiverDepository moov.Depository

	transfer moov.Transfer
}

var (
	logmu sync.Mutex // guards iterate(..) logging

	successfulTransfers = prometheus.NewCounterFrom(stdprometheus.CounterOpts{
		Name: "successful_ach_transfers",
		Help: "Counter of successful ACH transfers",
	}, []string{"source"})

	failedTransfers = prometheus.NewCounterFrom(stdprometheus.CounterOpts{
		Name: "failed_ach_transfers",
		Help: "Counter of failed ACH transfers",
	}, []string{"source"})
)

func iterate(ctx context.Context, requestID string) *iteration {
	var failureOncer sync.Once

	var lines []string
	debugLogger := func(tpl string, args ...interface{}) {
		if *flagFakeData {
			lines = append(lines, fmt.Sprintf(tpl, args...))
		} else {
			log.Printf(tpl, args...)
		}
	}
	errLogger := func(tpl string, args ...interface{}) {
		failureOncer.Do(func() {
			failedTransfers.With("source", "apitest").Add(1)
		})

		if *flagFakeData {
			lines = append(lines, fmt.Sprintf(tpl, args...))
		} else {
			log.Printf(tpl, args...)
		}
	}
	defer func() { // after an iteration print all logs at once
		logmu.Lock()
		defer logmu.Unlock()
		for i := range lines {
			log.Println(lines[i])
		}
		fmt.Println("")
	}()

	conf := makeConfiguration()
	conf.AddDefaultHeader("X-Request-ID", requestID)
	conf.AddDefaultHeader("Origin", "https://moov.io")
	debugLogger("Using X-Request-ID: %s", requestID)
	api := moov.NewAPIClient(conf)

	featureFlags, err := grabPaygateFeatures(flagLocal, *flagPaygateAdminAddress, adminHTTPClient)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}

	// Create our random user
	user, err := createUser(ctx, api)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Created user %s (email: %s)", user.ID, user.Email)

	// Add auth cookie and userId on every request from now on
	setMoovAuthCookie(conf, user)

	// Verify Cookie works
	if err := verifyUserIsLoggedIn(ctx, api, user); err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Cookie works for user %s", user.ID)

	oauthToken, err := createOAuthToken(ctx, api, user)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	expiresIn, _ := time.ParseDuration(fmt.Sprintf("%ds", oauthToken.ExpiresIn))
	if v := os.Getenv("TRAVIS_OS_NAME"); v != "" {
		// Hide our OAuth2 access_token from TravisCI logs...
		debugLogger("SUCCESS: Created OAuth access token, expires in %v", expiresIn)
	}
	debugLogger("SUCCESS: Created OAuth access token (%s), expires in %v", oauthToken.AccessToken, expiresIn)

	if *flagOAuth {
		debugLogger("Using OAuth for all requests now.")

		removeMoovAuthCookie(conf) // we only want OAuth credentials on requests
		setMoovOAuthToken(conf, oauthToken)
	}

	// Setup our micro-deposit origination account (or read its info if already setup)
	microDepositOrig, err := createMicroDepositAccount(ctx, api, user)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("INFO: micro-deposit account=%s", microDepositOrig.ID)

	// Create Originator Account
	// We create these accounts because they won't exist in the Accounts service already. (We're using fake data/accounts.)
	origAcct, err := createAccount(ctx, api, user, "from account", "")
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}

	// Create Originator Depository
	origDep, err := createDepository(ctx, api, user, origAcct)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Created Originator Depository (id=%s) for user", origDep.ID)

	// Create Originator
	orig, err := createOriginator(ctx, api, user, featureFlags, origDep.ID)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Created Originator (id=%s) for user", orig.ID)

	// By default with -local assume we want to approve customers.
	if !featureFlags.CustomersCallsDisabled {
		if err := attemptCustomerApproval(ctx, *flagCustomersAdminAddress, orig.CustomerID); err != nil {
			errLogger("FAILURE: %v", err)
			return nil
		} else {
			debugLogger("INFO: approved customer=%s", orig.CustomerID)
		}
	}

	// Create Receiver Account
	receiverAcct, err := createAccount(ctx, api, user, "to account", "")
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}

	// Create Receiver Depository
	receiverDep, err := createDepository(ctx, api, user, receiverAcct)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Created Receiver Depository (id=%s) for user", receiverDep.ID)

	// Create Receiver
	receiver, err := createReceiver(ctx, api, user, featureFlags, receiverDep.ID)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Created Receiver (id=%s) for user", receiver.ID)

	if !featureFlags.CustomersCallsDisabled {
		if err := attemptCustomerApproval(ctx, *flagCustomersAdminAddress, receiver.CustomerID); err != nil {
			errLogger("FAILURE: %v", err)
			return nil
		} else {
			debugLogger("INFO: approved customer=%s", receiver.CustomerID)
		}
	}

	// Create Transfer
	tx, err := createTransfer(ctx, api, receiver, orig, amount(), user.ID)
	if err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: Created %s transfer (id=%s) for user", tx.Amount, tx.ID)

	// Verify the Transaction was posted
	if !featureFlags.AccountsCallsDisabled {
		if err := checkTransactions(ctx, api, origAcct.ID, user, tx.Amount); err != nil {
			errLogger("FAILURE: %v", err)
			return nil
		}
		if err := checkTransactions(ctx, api, receiverAcct.ID, user, tx.Amount); err != nil {
			errLogger("FAILURE: %v", err)
			return nil
		}
		debugLogger("SUCCESS: Matched transactions on accounts")
	}

	// Attempt a Failed login
	if err := attemptFailedLogin(ctx, api); err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: invalid login credentials were rejected")

	// Attempt a Failed OAuth2 auth check
	if err := attemptFailedOAuth2Login(ctx, api); err != nil {
		errLogger("FAILURE: %v", err)
		return nil
	}
	debugLogger("SUCCESS: invalid OAuth2 access token was rejected")

	successfulTransfers.With("source", "apitest").Add(1)

	return &iteration{
		user:                 user,
		oauthToken:           *oauthToken,
		requestID:            requestID,
		userID:               user.ID,
		originator:           orig,
		originatorAccount:    origAcct,
		originatorDepository: origDep,
		receiver:             receiver,
		receiverAccount:      receiverAcct,
		receiverDepository:   receiverDep,
		transfer:             tx,
	}
}

// amount returns a random amount in string form accepted by the Moov API
func amount() string {
	n := float64(randSource.Int63()%2500) / 10.2 // max out at $250
	return fmt.Sprintf("USD %.2f", n)
}

// generateID creates a unique random string
func generateID() string {
	bs := make([]byte, 20)
	n, err := rand.Read(bs)
	if err != nil || n == 0 {
		return ""
	}
	return strings.ToLower(hex.EncodeToString(bs))
}
