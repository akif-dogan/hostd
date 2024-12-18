package contracts_test

import (
	"reflect"
	"testing"
	"time"

	"go.thebigfile.com/hostd/host/accounts"
	"go.thebigfile.com/hostd/host/contracts"
	"go.thebigfile.com/hostd/host/metrics"
	"go.thebigfile.com/hostd/internal/testutil"
	"go.thebigfile.com/hostd/persist/sqlite"
	rhp2 "go.thebigfile.com/core/rhp/v2"
	proto3 "go.thebigfile.com/core/rhp/v3"
	proto4 "go.thebigfile.com/core/rhp/v4"
	"go.thebigfile.com/core/types"
	"go.uber.org/goleak"
	"go.uber.org/zap/zaptest"
	"lukechampine.com/frand"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func assertRevenue(t *testing.T, s *sqlite.Store, potential, earned metrics.Revenue) {
	t.Helper()

	time.Sleep(time.Second) // commit time

	m, err := s.Metrics(time.Now())
	if err != nil {
		t.Fatalf("failed to get revenue metrics: %v", err)
	}

	actualPotentialValue := reflect.ValueOf(m.Revenue.Potential)
	expPotentialValue := reflect.ValueOf(potential)
	for i := 0; i < actualPotentialValue.NumField(); i++ {
		name := actualPotentialValue.Type().Field(i).Name
		fa, fe := actualPotentialValue.Field(i), expPotentialValue.Field(i)
		av, ev := fa.Interface().(types.Currency), fe.Interface().(types.Currency)

		if !av.Equals(ev) {
			t.Fatalf("potential revenue field %q does not match. expected %d, got %d", name, ev, av)
		}
	}

	actualEarnedValue := reflect.ValueOf(m.Revenue.Earned)
	expEarnedValue := reflect.ValueOf(earned)
	for i := 0; i < actualEarnedValue.NumField(); i++ {
		name := actualEarnedValue.Type().Field(i).Name
		fa, fe := actualEarnedValue.Field(i), expEarnedValue.Field(i)
		av, ev := fa.Interface().(types.Currency), fe.Interface().(types.Currency)

		if !av.Equals(ev) {
			t.Fatalf("earned revenue field %q does not match. expected %d, got %d", name, ev, av)
		}
	}
}

func TestRevenueMetrics(t *testing.T) {
	t.Run("successful", func(t *testing.T) {
		log := zaptest.NewLogger(t)
		renterKey, hostKey := types.GeneratePrivateKey(), types.GeneratePrivateKey()
		network, genesis := testutil.V1Network()
		host := testutil.NewHostNode(t, hostKey, network, genesis, log)
		cm := host.Contracts

		// fund the wallet
		testutil.MineAndSync(t, host, host.Wallet.Address(), int(network.MaturityDelay)+10)

		var expectedPotential, expectedEarned metrics.Revenue

		settings, err := host.Settings.RHP2Settings()
		if err != nil {
			t.Fatal(err)
		}

		revision := formContract(t, host.Chain, host.Contracts, host.Wallet, host.Syncer, host.Settings, renterKey, hostKey, types.Siacoins(100), types.Siacoins(200), 10, true)

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		reviseContract := func(t *testing.T, usage contracts.Usage) {
			t.Helper()

			updater, err := cm.ReviseContract(revision.Revision.ParentID)
			if err != nil {
				t.Fatal("failed to update contract:", err)
			}
			defer updater.Close()

			fc := revision.Revision
			// adjust the payouts so the host will broadcast a proof
			total := usage.RPCRevenue.Add(usage.StorageRevenue).Add(usage.IngressRevenue).Add(usage.EgressRevenue).Add(usage.AccountFunding)
			fc.ValidProofOutputs = append([]types.SiacoinOutput(nil), fc.ValidProofOutputs...)
			fc.ValidProofOutputs[0].Value = fc.ValidProofOutputs[0].Value.Sub(total)
			fc.ValidProofOutputs[1].Value = fc.ValidProofOutputs[1].Value.Add(total)
			fc.RevisionNumber++
			sigHash := hashRevision(fc)
			revision = contracts.SignedRevision{
				Revision:        fc,
				HostSignature:   hostKey.SignHash(sigHash),
				RenterSignature: renterKey.SignHash(sigHash),
			}
			if err := updater.Commit(revision, usage); err != nil {
				t.Fatal("failed to commit contract revision:", err)
			}
		}

		reviseContract(t, contracts.Usage{
			StorageRevenue: types.Siacoins(1),
		})

		// mine until the contract is active
		testutil.MineAndSync(t, host, host.Wallet.Address(), 1)

		expectedPotential.RPC = expectedPotential.RPC.Add(settings.ContractPrice)
		expectedPotential.Storage = expectedPotential.Storage.Add(types.Siacoins(1))
		// check that the revenue metrics were updated
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		reviseContract(t, contracts.Usage{
			IngressRevenue: types.NewCurrency64(1000),
		})
		expectedPotential.Ingress = expectedPotential.Ingress.Add(types.NewCurrency64(1000))

		// fund an account
		accountID := proto3.Account(renterKey.PublicKey())
		_, err = host.Accounts.Credit(accounts.FundAccountWithContract{
			Account:    accountID,
			Cost:       types.NewCurrency64(1),
			Amount:     types.Siacoins(1),
			Revision:   revision,
			Expiration: time.Now().Add(time.Hour),
		}, false)
		if err != nil {
			t.Fatal(err)
		}
		expectedPotential.RPC = expectedPotential.RPC.Add(types.NewCurrency64(1))

		// mine until the contract is successful
		testutil.MineAndSync(t, host, types.VoidAddress, int(revision.Revision.WindowEnd-host.Chain.Tip().Height+1))

		// check that the revenue metrics were updated
		expectedEarned = expectedPotential
		expectedPotential = metrics.Revenue{}
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		// spend from the account
		b, err := host.Accounts.Budget(accountID, types.Siacoins(1).Div64(5))
		if err != nil {
			t.Fatal(err)
		}

		err = b.Spend(accounts.Usage{
			IngressRevenue: types.Siacoins(1).Div64(10),
			EgressRevenue:  types.Siacoins(1).Div64(10),
		})
		if err != nil {
			t.Fatal(err)
		} else if err := b.Commit(); err != nil {
			t.Fatal(err)
		}

		expectedEarned.Ingress = expectedEarned.Ingress.Add(types.Siacoins(1).Div64(10))
		expectedEarned.Egress = expectedEarned.Egress.Add(types.Siacoins(1).Div64(10))
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)
	})

	t.Run("failed", func(t *testing.T) {
		log := zaptest.NewLogger(t)
		renterKey, hostKey := types.GeneratePrivateKey(), types.GeneratePrivateKey()
		network, genesis := testutil.V1Network()
		host := testutil.NewHostNode(t, hostKey, network, genesis, log)
		cm := host.Contracts

		// fund the wallet
		testutil.MineAndSync(t, host, host.Wallet.Address(), int(network.MaturityDelay)+10)

		var expectedPotential, expectedEarned metrics.Revenue

		settings, err := host.Settings.RHP2Settings()
		if err != nil {
			t.Fatal(err)
		}

		revision := formContract(t, host.Chain, host.Contracts, host.Wallet, host.Syncer, host.Settings, renterKey, hostKey, types.Siacoins(100), types.Siacoins(200), 10, true)

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		reviseContract := func(t *testing.T, usage contracts.Usage) {
			t.Helper()

			updater, err := cm.ReviseContract(revision.Revision.ParentID)
			if err != nil {
				t.Fatal("failed to update contract:", err)
			}
			defer updater.Close()

			fc := revision.Revision
			// corrupt the contract data so the proof fails
			fc.Filesize = proto4.SectorSize
			fc.FileMerkleRoot = frand.Entropy256()
			// adjust the payouts so the host will attempt to broadcast a proof
			total := usage.RPCRevenue.Add(usage.StorageRevenue).Add(usage.IngressRevenue).Add(usage.EgressRevenue).Add(usage.AccountFunding)
			fc.ValidProofOutputs = append([]types.SiacoinOutput(nil), fc.ValidProofOutputs...)
			fc.ValidProofOutputs[0].Value = fc.ValidProofOutputs[0].Value.Sub(total)
			fc.ValidProofOutputs[1].Value = fc.ValidProofOutputs[1].Value.Add(total)
			fc.RevisionNumber++
			sigHash := hashRevision(fc)
			revision = contracts.SignedRevision{
				Revision:        fc,
				HostSignature:   hostKey.SignHash(sigHash),
				RenterSignature: renterKey.SignHash(sigHash),
			}
			if err := updater.Commit(revision, usage); err != nil {
				t.Fatal("failed to commit contract revision:", err)
			}
		}

		reviseContract(t, contracts.Usage{
			StorageRevenue: types.Siacoins(1),
		})

		// mine until the contract is active
		testutil.MineAndSync(t, host, host.Wallet.Address(), 1)

		expectedPotential.RPC = expectedPotential.RPC.Add(settings.ContractPrice)
		expectedPotential.Storage = expectedPotential.Storage.Add(types.Siacoins(1))
		// check that the revenue metrics were updated
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		// fund an account
		accountID := proto3.Account(renterKey.PublicKey())
		_, err = host.Accounts.Credit(accounts.FundAccountWithContract{
			Account:    accountID,
			Cost:       types.NewCurrency64(1),
			Amount:     types.Siacoins(1),
			Revision:   revision,
			Expiration: time.Now().Add(time.Hour),
		}, false)
		if err != nil {
			t.Fatal(err)
		}
		expectedPotential.RPC = expectedPotential.RPC.Add(types.NewCurrency64(1))
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		// mine until the contract has expired
		testutil.MineAndSync(t, host, types.VoidAddress, int(revision.Revision.WindowEnd-host.Chain.Tip().Height+1))

		// check that the revenue metrics are empty
		expectedPotential = metrics.Revenue{}
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)

		// spend from the account
		b, err := host.Accounts.Budget(accountID, types.Siacoins(1).Div64(5))
		if err != nil {
			t.Fatal(err)
		}

		err = b.Spend(accounts.Usage{
			IngressRevenue: types.Siacoins(1).Div64(10),
			EgressRevenue:  types.Siacoins(1).Div64(10),
		})
		if err != nil {
			t.Fatal(err)
		} else if err := b.Commit(); err != nil {
			t.Fatal(err)
		}

		// check that the revenue metrics are still empty
		assertRevenue(t, host.Store, expectedPotential, expectedEarned)
	})

	t.Run("rejected", func(t *testing.T) {
		log := zaptest.NewLogger(t)
		renterKey, hostKey := types.GeneratePrivateKey(), types.GeneratePrivateKey()
		network, genesis := testutil.V1Network()
		host := testutil.NewHostNode(t, hostKey, network, genesis, log)
		cm := host.Contracts

		// fund the wallet
		testutil.MineAndSync(t, host, host.Wallet.Address(), int(network.MaturityDelay)+10)

		renterFunds := types.Siacoins(100)
		hostFunds := types.Siacoins(200)

		contract := rhp2.PrepareContractFormation(renterKey.PublicKey(), hostKey.PublicKey(), renterFunds, hostFunds, host.Chain.Tip().Height+10, rhp2.HostSettings{WindowSize: 10}, host.Wallet.Address())
		state := host.Chain.TipState()
		formationCost := rhp2.ContractFormationCost(state, contract, types.ZeroCurrency)
		contractUnlockConditions := types.UnlockConditions{
			PublicKeys: []types.UnlockKey{
				renterKey.PublicKey().UnlockKey(),
				hostKey.PublicKey().UnlockKey(),
			},
			SignaturesRequired: 2,
		}
		txn := types.Transaction{
			FileContracts: []types.FileContract{contract},
		}
		toSign, err := host.Wallet.FundTransaction(&txn, formationCost.Add(hostFunds), true) // we're funding both sides of the payout
		if err != nil {
			t.Fatal("failed to fund transaction:", err)
		}
		host.Wallet.SignTransaction(&txn, toSign, types.CoveredFields{WholeTransaction: true})
		formationSet := append(host.Chain.UnconfirmedParents(txn), txn)
		fcr := types.FileContractRevision{
			ParentID:         txn.FileContractID(0),
			UnlockConditions: contractUnlockConditions,
			FileContract:     txn.FileContracts[0],
		}
		// corrupt the transaction set to simulate a rejected contract
		formationSet[len(formationSet)-1].Signatures = nil
		fcr.RevisionNumber = 1
		sigHash := hashRevision(fcr)
		revision := contracts.SignedRevision{
			Revision:        fcr,
			HostSignature:   hostKey.SignHash(sigHash),
			RenterSignature: renterKey.SignHash(sigHash),
		}
		if err := host.Contracts.AddContract(revision, formationSet, hostFunds, contracts.Usage{}); err != nil {
			t.Fatal(err)
		}

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, metrics.Revenue{}, metrics.Revenue{})

		reviseContract := func(t *testing.T, usage contracts.Usage) {
			t.Helper()

			updater, err := cm.ReviseContract(revision.Revision.ParentID)
			if err != nil {
				t.Fatal("failed to update contract:", err)
			}
			defer updater.Close()

			fc := revision.Revision
			// adjust the payouts so the host will attempt to broadcast a proof
			total := usage.RPCRevenue.Add(usage.StorageRevenue).Add(usage.IngressRevenue).Add(usage.EgressRevenue).Add(usage.AccountFunding)
			fc.ValidProofOutputs = append([]types.SiacoinOutput(nil), fc.ValidProofOutputs...)
			fc.ValidProofOutputs[0].Value = fc.ValidProofOutputs[0].Value.Sub(total)
			fc.ValidProofOutputs[1].Value = fc.ValidProofOutputs[1].Value.Add(total)
			fc.RevisionNumber++
			sigHash := hashRevision(fc)
			revision = contracts.SignedRevision{
				Revision:        fc,
				HostSignature:   hostKey.SignHash(sigHash),
				RenterSignature: renterKey.SignHash(sigHash),
			}
			if err := updater.Commit(revision, usage); err != nil {
				t.Fatal("failed to commit contract revision:", err)
			}
		}

		reviseContract(t, contracts.Usage{
			StorageRevenue: types.Siacoins(1),
		})

		// mine a block, the contract should not be active
		testutil.MineAndSync(t, host, host.Wallet.Address(), 1)

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, metrics.Revenue{}, metrics.Revenue{})

		// fund an account
		accountID := proto3.Account(renterKey.PublicKey())
		_, err = host.Accounts.Credit(accounts.FundAccountWithContract{
			Account:    accountID,
			Cost:       types.NewCurrency64(1),
			Amount:     types.Siacoins(1),
			Revision:   revision,
			Expiration: time.Now().Add(time.Hour),
		}, false)
		if err != nil {
			t.Fatal(err)
		}

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, metrics.Revenue{}, metrics.Revenue{})

		// mine until the contract is successful
		testutil.MineAndSync(t, host, types.VoidAddress, int(revision.Revision.WindowEnd-host.Chain.Tip().Height+1))

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, metrics.Revenue{}, metrics.Revenue{})

		// spend from the account
		b, err := host.Accounts.Budget(accountID, types.Siacoins(1).Div64(5))
		if err != nil {
			t.Fatal(err)
		}

		err = b.Spend(accounts.Usage{
			IngressRevenue: types.Siacoins(1).Div64(10),
			EgressRevenue:  types.Siacoins(1).Div64(10),
		})
		if err != nil {
			t.Fatal(err)
		} else if err := b.Commit(); err != nil {
			t.Fatal(err)
		}

		// contract revenue is not expected until the contract is active
		assertRevenue(t, host.Store, metrics.Revenue{}, metrics.Revenue{})
	})
}
