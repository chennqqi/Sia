package wallet

import (
	"crypto/rand"
	"errors"
	"runtime"
	"sync"

	"github.com/NebulousLabs/Sia/crypto"
	"github.com/NebulousLabs/Sia/encoding"
	"github.com/NebulousLabs/Sia/modules"
	"github.com/NebulousLabs/Sia/types"
	"github.com/NebulousLabs/bolt"
)

var (
	errKnownSeed = errors.New("seed is already known")
)

type (
	// uniqueID is a unique id randomly generated and put at the front of every
	// persistence object. It is used to make sure that a different encryption
	// key can be used for every persistence object.
	uniqueID [crypto.EntropySize]byte

	// seedFile stores an encrypted wallet seed on disk.
	seedFile struct {
		UID                    uniqueID
		EncryptionVerification crypto.Ciphertext
		Seed                   crypto.Ciphertext
	}
)

// generateSpendableKey creates the keys and unlock conditions for seed at a
// given index.
func generateSpendableKey(seed modules.Seed, index uint64) spendableKey {
	sk, pk := crypto.GenerateKeyPairDeterministic(crypto.HashAll(seed, index))
	return spendableKey{
		UnlockConditions: types.UnlockConditions{
			PublicKeys:         []types.SiaPublicKey{types.Ed25519PublicKey(pk)},
			SignaturesRequired: 1,
		},
		SecretKeys: []crypto.SecretKey{sk},
	}
}

// generateKeys generates n keys from seed, starting from index start.
func generateKeys(seed modules.Seed, start, n uint64) []spendableKey {
	// generate in parallel, one goroutine per core.
	keys := make([]spendableKey, n)
	var wg sync.WaitGroup
	wg.Add(runtime.NumCPU())
	for cpu := 0; cpu < runtime.NumCPU(); cpu++ {
		go func(offset uint64) {
			defer wg.Done()
			for i := offset; i < n; i += uint64(runtime.NumCPU()) {
				// NOTE: don't bother trying to optimize generateSpendableKey;
				// profiling shows that ed25519 key generation consumes far
				// more CPU time than encoding or hashing.
				keys[i] = generateSpendableKey(seed, start+i)
			}
		}(uint64(cpu))
	}
	wg.Wait()
	return keys
}

// createSeedFile creates and encrypts a seedFile.
func createSeedFile(masterKey crypto.TwofishKey, seed modules.Seed) (seedFile, error) {
	var sf seedFile
	_, err := rand.Read(sf.UID[:])
	if err != nil {
		return seedFile{}, err
	}
	sek := uidEncryptionKey(masterKey, sf.UID)
	sf.EncryptionVerification, err = sek.EncryptBytes(verificationPlaintext)
	if err != nil {
		return seedFile{}, err
	}
	sf.Seed, err = sek.EncryptBytes(seed[:])
	if err != nil {
		return seedFile{}, err
	}
	return sf, nil
}

// decryptSeedFile decrypts a seed file using the encryption key.
func decryptSeedFile(masterKey crypto.TwofishKey, sf seedFile) (seed modules.Seed, err error) {
	// Verify that the provided master key is the correct key.
	decryptionKey := uidEncryptionKey(masterKey, sf.UID)
	err = verifyEncryption(decryptionKey, sf.EncryptionVerification)
	if err != nil {
		return modules.Seed{}, err
	}

	// Decrypt and return the seed.
	plainSeed, err := decryptionKey.DecryptBytes(sf.Seed)
	if err != nil {
		return modules.Seed{}, err
	}
	copy(seed[:], plainSeed)
	return seed, nil
}

// integrateSeed generates n spendableKeys from the seed and loads them into
// the wallet.
func (w *Wallet) integrateSeed(seed modules.Seed, n uint64) {
	for _, sk := range generateKeys(seed, 0, n) {
		w.keys[sk.UnlockConditions.UnlockHash()] = sk
	}
}

// loadSeed integrates a recovery seed into the wallet.
func (w *Wallet) loadSeed(masterKey crypto.TwofishKey, seed modules.Seed) error {
	// Because the recovery seed does not have a UID, duplication must be
	// prevented by comparing with the list of decrypted seeds. This can only
	// occur while the wallet is unlocked.
	if !w.unlocked {
		return modules.ErrLockedWallet
	}
	if seed == w.primarySeed {
		return errKnownSeed
	}
	for _, wSeed := range w.seeds {
		if seed == wSeed {
			return errKnownSeed
		}
	}

	err := w.db.Update(func(tx *bolt.Tx) error {
		err := checkMasterKey(tx, masterKey)
		if err != nil {
			return err
		}

		// create a seedFile for the seed
		sf, err := createSeedFile(masterKey, seed)
		if err != nil {
			return err
		}

		// add the seedFile
		return tx.Bucket(bucketSeedFiles).Put(sf.UID[:], encoding.Marshal(sf))
	})
	if err != nil {
		return err
	}

	// load the seed's keys
	w.integrateSeed(seed, modules.PublicKeysPerSeed)
	w.seeds = append(w.seeds, seed)
	return nil
}

// nextPrimarySeedAddress fetches the next address from the primary seed.
func (w *Wallet) nextPrimarySeedAddress(tx *bolt.Tx) (types.UnlockConditions, error) {
	// Check that the wallet has been unlocked.
	if !w.unlocked {
		return types.UnlockConditions{}, modules.ErrLockedWallet
	}

	// Fetch and increment the seed progress.
	progress, err := dbGetPrimarySeedProgress(tx)
	if err != nil {
		return types.UnlockConditions{}, err
	}
	if err = dbPutPrimarySeedProgress(tx, progress+1); err != nil {
		return types.UnlockConditions{}, err
	}
	// Integrate the next key into the wallet, and return the unlock
	// conditions.
	spendableKey := generateSpendableKey(w.primarySeed, progress)
	w.keys[spendableKey.UnlockConditions.UnlockHash()] = spendableKey
	return spendableKey.UnlockConditions, nil
}

// AllSeeds returns a list of all seeds known to and used by the wallet.
func (w *Wallet) AllSeeds() ([]modules.Seed, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.unlocked {
		return nil, modules.ErrLockedWallet
	}
	return append([]modules.Seed{w.primarySeed}, w.seeds...), nil
}

// PrimarySeed returns the decrypted primary seed of the wallet, as well as
// the number of addresses that the seed can be safely used to generate.
func (w *Wallet) PrimarySeed() (modules.Seed, uint64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.unlocked {
		return modules.Seed{}, 0, modules.ErrLockedWallet
	}
	// TODO: going to the db is slow; consider caching progress. On the other
	// hand, PrimarySeed isn't a frequently called method, so caching may be
	// overkill.
	var progress uint64
	err := w.db.View(func(tx *bolt.Tx) error {
		var err error
		progress, err = dbGetPrimarySeedProgress(tx)
		return err
	})
	if err != nil {
		return modules.Seed{}, 0, err
	}

	// addresses remaining is maxScanKeys-progress; generating more keys than
	// that risks not being able to recover them when using SweepSeed or
	// InitFromSeed.
	remaining := maxScanKeys - progress
	if progress > maxScanKeys {
		remaining = 0
	}
	return w.primarySeed, remaining, nil
}

// NextAddress returns an unlock hash that is ready to receive siacoins or
// siafunds. The address is generated using the primary address seed.
func (w *Wallet) NextAddress() (types.UnlockConditions, error) {
	if err := w.tg.Add(); err != nil {
		return types.UnlockConditions{}, err
	}
	defer w.tg.Done()
	w.mu.Lock()
	defer w.mu.Unlock()

	// TODO: going to the db is slow; consider creating 100 addresses at a
	// time.
	var uc types.UnlockConditions
	err := w.db.Update(func(tx *bolt.Tx) error {
		var err error
		uc, err = w.nextPrimarySeedAddress(tx)
		return err
	})
	return uc, err
}

// LoadSeed will track all of the addresses generated by the input seed,
// reclaiming any funds that were lost due to a deleted file or lost encryption
// key. An error will be returned if the seed has already been integrated with
// the wallet.
func (w *Wallet) LoadSeed(masterKey crypto.TwofishKey, seed modules.Seed) error {
	if err := w.tg.Add(); err != nil {
		return err
	}
	defer w.tg.Done()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.loadSeed(masterKey, seed)
}

// SweepSeed scans the blockchain for outputs generated from seed and creates
// a transaction that transfers them to the wallet. Note that this incurs a
// transaction fee. It returns the total value of the outputs, minus the fee.
// If only siafunds were found, the fee is deducted from the wallet.
func (w *Wallet) SweepSeed(seed modules.Seed) (coins, funds types.Currency, err error) {
	if err = w.tg.Add(); err != nil {
		return
	}
	defer w.tg.Done()

	w.mu.RLock()
	match := seed == w.primarySeed
	w.mu.RUnlock()
	if match {
		return types.Currency{}, types.Currency{}, errors.New("cannot sweep primary seed")
	}

	if !w.cs.Synced() {
		return types.Currency{}, types.Currency{}, errors.New("cannot sweep until blockchain is synced")
	}

	// get an address to spend into
	var uc types.UnlockConditions
	err = w.db.Update(func(tx *bolt.Tx) error {
		var err error
		uc, err = w.nextPrimarySeedAddress(tx)
		return err
	})
	if err != nil {
		return
	}

	// scan blockchain for outputs, filtering out 'dust' (outputs that cost
	// more in fees than they are worth)
	s := newSeedScanner(seed)
	_, maxFee := w.tpool.FeeEstimation()
	s.dustThreshold = maxFee.Mul64(outputSize)
	if err = s.scan(w.cs); err != nil {
		return
	}

	scos := make([]scannedOutput, 0, len(s.siacoinOutputs))
	sfos := make([]scannedOutput, 0, len(s.siafundOutputs))
	for _, output := range s.siacoinOutputs {
		scos = append(scos, output)
	}
	for _, output := range s.siafundOutputs {
		sfos = append(sfos, output)
	}
	return w.sweepOutputs(scos, sfos)
}
