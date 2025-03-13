package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"time"

	"github.com/codenotary/immudb/embedded/store"
)

func exitOnErr(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	numAccounts := flag.Int("num-accounts", 100, "number of accounts")
	balance := flag.Int("balance", 1000, "initial account balance")
	duration := flag.Duration("duraton", 10*time.Minute, "test duration")

	flag.Parse()

	indexOpts := store.DefaultIndexOptions().WithMaxActiveSnapshots(*numAccounts + 1)
	st, err := store.Open(os.TempDir(), store.DefaultOptions().WithMaxConcurrency(*numAccounts).WithIndexOptions(indexOpts))
	exitOnErr(err)
	defer st.Close()

	ledger, err := st.OpenLedger("default")
	exitOnErr(err)
	defer ledger.Close()

	createAccounts(ledger, *numAccounts, *balance)
	ledger.WaitForIndexingUpto(context.Background(), 1)

	go func() {
		for {
			checkBalances(ledger, *numAccounts*(*balance))

			time.Sleep(time.Millisecond * 10)
		}
	}()

	go func() {
		for {
			makeTransfers(ledger, *numAccounts)
		}
	}()

	time.Sleep(*duration)
}

func checkBalances(ledger *store.Ledger, totalBalance int) {
	tx, err := ledger.NewTx(context.Background(), store.DefaultTxOptions().WithMode(store.ReadOnlyTx))
	exitOnErr(err)
	defer tx.Cancel()

	reader, err := tx.NewKeyReader(store.KeyReaderSpec{})
	exitOnErr(err)

	defer reader.Close()

	balance := uint64(0)
	for {
		_, val, err := reader.Read(context.Background())
		if errors.Is(err, store.ErrNoMoreEntries) {
			break
		}

		value, err := ledger.Resolve(val)
		exitOnErr(err)

		balance += binary.BigEndian.Uint64(value)
	}

	if balance != uint64(totalBalance) {
		panic(fmt.Sprintf("total balance should be %d, but is %d", balance, totalBalance))
	}
}

func makeTransfers(ledger *store.Ledger, numAccounts int) {
	var wg sync.WaitGroup
	wg.Add(numAccounts)

	for i := 0; i < numAccounts; i++ {
		go func() {
			defer wg.Done()

			src := rand.Intn(numAccounts)
			dst := rand.Intn(numAccounts)

			srcAccount := getAccountKey(src)
			dstAccount := getAccountKey(dst)

			tx, err := ledger.NewTx(context.Background(), store.DefaultTxOptions())
			exitOnErr(err)
			defer tx.Cancel()

			vref, err := tx.Get(context.Background(), srcAccount)
			exitOnErr(err)

			value, err := ledger.Resolve(vref)
			exitOnErr(err)

			amount := uint64(1 + rand.Intn(10))

			err = tx.Set(srcAccount, nil, addValue(value, -int64(amount)))
			exitOnErr(err)

			vref, err = tx.Get(context.Background(), dstAccount)
			exitOnErr(err)

			value, err = ledger.Resolve(vref)
			exitOnErr(err)

			err = tx.Set(dstAccount, nil, addValue(value, int64(amount)))
			exitOnErr(err)

			_, err = tx.Commit(context.Background())
			if !errors.Is(err, store.ErrTxReadConflict) {
				exitOnErr(err)
			}
		}()
	}
	wg.Wait()
}

func createAccounts(ledger *store.Ledger, n int, initialBalance int) {
	tx, err := ledger.NewTx(context.Background(), store.DefaultTxOptions())
	exitOnErr(err)
	defer tx.Cancel()

	for i := 0; i < n; i++ {
		var balance [8]byte
		binary.BigEndian.PutUint64(balance[:], uint64(initialBalance))

		err := tx.Set(getAccountKey(i), nil, balance[:])
		exitOnErr(err)
	}

	_, err = tx.Commit(context.Background())
	exitOnErr(err)
}

func getAccountKey(i int) []byte {
	return fmt.Appendf(nil, "account-%d", i)
}

func addValue(v []byte, x int64) []byte {
	balance := binary.BigEndian.Uint64(v)

	var buf [8]byte

	newBalance := int64(balance) + int64(x)
	binary.BigEndian.PutUint64(buf[:], uint64(newBalance))

	return buf[:]
}
