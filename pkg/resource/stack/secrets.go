// Copyright 2016-2022, Pulumi Corporation.
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

package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/secrets/cloud"
	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
	"github.com/pulumi/pulumi/pkg/v3/secrets/service"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/env"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
)

// DefaultSecretsProvider is the default SecretsProvider to use when deserializing deployments.
var DefaultSecretsProvider secrets.Provider = &defaultSecretsProvider{}

// defaultSecretsProvider implements the secrets.ManagerProviderFactory interface. Essentially
// it is the global location where new secrets managers can be registered for use when
// decrypting checkpoints.
type defaultSecretsProvider struct{}

// OfType returns a secrets manager for the given secrets type. Returns an error
// if the type is unknown or the state is invalid.
func (defaultSecretsProvider) OfType(ty string, state json.RawMessage) (secrets.Manager, error) {
	var sm secrets.Manager
	var err error
	switch ty {
	case passphrase.Type:
		sm, err = passphrase.NewPromptingPassphraseSecretsManagerFromState(state)
	case service.Type:
		sm, err = service.NewServiceSecretsManagerFromState(state)
	case cloud.Type:
		sm, err = cloud.NewCloudSecretsManagerFromState(state)
	default:
		return nil, fmt.Errorf("no known secrets provider for type %q", ty)
	}
	if err != nil {
		return nil, fmt.Errorf("constructing secrets manager of type %q: %w", ty, err)
	}

	return NewBatchingCachingSecretsManager(sm), nil
}

// NamedStackSecretsProvider is the same as the default secrets provider,
// but is aware of the stack name for which it is used.  Currently
// this is only used for prompting passphrase secrets managers to show
// the stackname in the prompt for the passphrase.
type NamedStackSecretsProvider struct {
	StackName string
}

// OfType returns a secrets manager for the given secrets type. Returns an error
// if the type is unknown or the state is invalid.
func (s NamedStackSecretsProvider) OfType(ty string, state json.RawMessage) (secrets.Manager, error) {
	var sm secrets.Manager
	var err error
	switch ty {
	case passphrase.Type:
		sm, err = passphrase.NewStackPromptingPassphraseSecretsManagerFromState(state, s.StackName)
	case service.Type:
		sm, err = service.NewServiceSecretsManagerFromState(state)
	case cloud.Type:
		sm, err = cloud.NewCloudSecretsManagerFromState(state)
	default:
		return nil, fmt.Errorf("no known secrets provider for type %q", ty)
	}
	if err != nil {
		return nil, fmt.Errorf("constructing secrets manager of type %q: %w", ty, err)
	}

	return NewBatchingCachingSecretsManager(sm), nil
}

// BatchingSecretsManager is a secrets.Manager that supports batch encryption and decryption operations.
type BatchingSecretsManager interface {
	secrets.Manager
	// BeginBatchEncryption returns a new BatchEncrypter and CompleteCrypterBatch function.
	// The BatchEncrypter allows encryption operations to be enqueued then processed in a batch request to avoid
	// round-trips for when doing so with the underlying encrypter incurs overhead. Encryptions to a secret are enqueued
	// and processed in a batch request either when the wrapping transaction is committed or earlier if an implementation
	// supports or requires it.
	//
	// The CompleteCrypterBatch function must be called to ensure that all enqueued encryption operations are processed.
	//
	//	batchEncrypter, completeCrypterBatch := batchingSecretsManager.BeginBatchEncryption()
	//	err = batchEncrypter.Enqueue(ctx, sourceSecret, plaintext, targetSerializedSecret)
	//	err = completeCrypterBatch(ctx)
	BeginBatchEncryption() (BatchEncrypter, CompleteCrypterBatch)
	// BeginBatchDecryption returns a new BatchDecrypter and CompleteCrypterBatch function.
	// The BatchDecrypter allows decryption operations to be batched to avoid round-trips for when the underlying
	// decrypter incurs overhead. Decryptions to a secret are enqueued and processed in a batch request either when the
	// queue is full or earlier if an implementation supports or requires it.
	//
	// The CompleteCrypterBatch function must be called to ensure that all enqueued decryption operations are processed.
	//
	//	batchDecrypter, completeCrypterBatch := batchingSecretsManager.BeginBatchDecryption()
	//	err = batchDecrypter.Enqueue(ctx, ciphertext, targetSecret)
	//	err = completeCrypterBatch(ctx)
	BeginBatchDecryption() (BatchDecrypter, CompleteCrypterBatch)
}

type batchingCachingSecretsManager struct {
	manager secrets.Manager
	cache   SecretCache
}

// NewBatchingCachingSecretsManager returns a new BatchingSecretsManager that caches the ciphertext for secret property
// values. A secrets.Manager that will be used to encrypt and decrypt values stored in a serialized deployment can be
// wrapped in a caching secrets manager in order to avoid re-encrypting secrets each time the deployment is serialized.
// When secrets values are not cached, then operations can be batched when using the batch transaction methods.
func NewBatchingCachingSecretsManager(manager secrets.Manager) BatchingSecretsManager {
	sm := &batchingCachingSecretsManager{
		manager: manager,
		cache:   NewSecretCache(),
	}
	return sm
}

func (csm *batchingCachingSecretsManager) Type() string {
	return csm.manager.Type()
}

func (csm *batchingCachingSecretsManager) State() json.RawMessage {
	return csm.manager.State()
}

func (csm *batchingCachingSecretsManager) Encrypter() config.Encrypter {
	return csm.manager.Encrypter()
}

func (csm *batchingCachingSecretsManager) Decrypter() config.Decrypter {
	return csm.manager.Decrypter()
}

func (csm *batchingCachingSecretsManager) BeginBatchEncryption() (BatchEncrypter, CompleteCrypterBatch) {
	return BeginBatchEncryptionWithCache(csm.manager.Encrypter(), csm.cache)
}

func (csm *batchingCachingSecretsManager) BeginBatchDecryption() (BatchDecrypter, CompleteCrypterBatch) {
	// We don't use the cache here to ensure that we always re-encrypt all secrets at least once per operation.
	return BeginDecryptionBatch(csm.manager.Decrypter())
}

// SecretCache allows the bidirectional cached conversion between: `ciphertext <-> plaintext + secret pointer`.
// The same plaintext can be associated with multiple secrets, each of which will have their own ciphertexts which
// should not be shared.
//
//	cache := NewSecretCache()
//	secret := &resource.Secret{}
//	cache.Write("plaintext", "ciphertext", secret)
//	plaintext, ok := cache.LookupPlaintext("ciphertext") // "plaintext", true
//	ciphertext, ok := cache.LookupCiphertext(secret, "plaintext") // "ciphertext", true
type SecretCache interface {
	// Write stores the plaintext, ciphertext, and secret in the cache, overwriting any previous entry for the secret.
	Write(plaintext, ciphertext string, secret *resource.Secret)
	// LookupCiphertext returns the cached ciphertext for the given secret and plaintext, if it exists.
	LookupCiphertext(secret *resource.Secret, plaintext string) (string, bool)
	// LookupPlaintext returns the cached plaintext for the given ciphertext, if it exists.
	LookupPlaintext(ciphertext string) (string, bool)
}

type nullSecretCache struct{}

func (nullSecretCache) Write(plaintext, ciphertext string, secret *resource.Secret) {}
func (nullSecretCache) LookupCiphertext(secret *resource.Secret, plaintext string) (string, bool) {
	return "", false
}

func (nullSecretCache) LookupPlaintext(ciphertext string) (string, bool) {
	return "", false
}

type secretCache struct {
	bySecret     sync.Map
	byCiphertext sync.Map
}

type secretCacheEntry struct {
	plaintext  string
	ciphertext string
	secret     *resource.Secret
}

// NewSecretCache returns a new secretCache which allows the bidirectional cached conversion between:
// `ciphertext <-> plaintext + secret pointer`. The same plaintext can be associated with multiple secrets, each of
// which will have their own ciphertexts which should not be shared.
//
// All methods are thread-safe and can be called concurrently by multiple goroutines.
// The cache can be disabled by setting the environment variable PULUMI_DISABLE_SECRET_CACHE to "true".
//
//	cache := NewSecretCache()
//	secret := &resource.Secret{}
//	cache.Write("plaintext", "ciphertext", secret)
//	plaintext, ok := cache.LookupPlaintext("ciphertext") // "plaintext", true
//	ciphertext, ok := cache.LookupCiphertext(secret, "plaintext") // "ciphertext", true
func NewSecretCache() SecretCache {
	// If the environment variable PULUMI_DISABLE_SECRET_CACHE is set to "true", the secret cache will be disabled and
	// no entries will be stored. This is a short-term escape hatch in case there's unforeseen issues with expanded
	// caching scopes and should be removable once we're confident that customers are not affected.
	if env.DisableSecretCache.Value() {
		return nullSecretCache{}
	}
	return &secretCache{}
}

// Write stores the plaintext, ciphertext, and secret in the cache, overwriting any previous entry for the secret.
// This method is thread-safe and can be called concurrently by multiple goroutines.
func (c *secretCache) Write(plaintext, ciphertext string, secret *resource.Secret) {
	entry := secretCacheEntry{plaintext, ciphertext, secret}
	c.bySecret.Store(secret, entry)
	c.byCiphertext.Store(ciphertext, entry)
}

// LookupCiphertext returns the cached ciphertext for the given secret and plaintext, if it exists.
// The ciphertext is returned as a string, and a boolean is returned to indicate whether the secret was found.
// This method is thread-safe and can be called concurrently by multiple goroutines.
func (c *secretCache) LookupCiphertext(secret *resource.Secret, plaintext string) (string, bool) {
	entry, ok := c.bySecret.Load(secret)
	if !ok {
		return "", false
	}
	cacheEntry := entry.(secretCacheEntry)
	if cacheEntry.plaintext != plaintext {
		return "", false
	}
	return cacheEntry.ciphertext, true
}

// LookupPlaintext returns the cached plaintext for the given ciphertext, if it exists.
// The plaintext is returned as a string, and a boolean is returned to indicate whether the ciphertext was found.
// This method is thread-safe and can be called concurrently by multiple goroutines.
func (c *secretCache) LookupPlaintext(ciphertext string) (string, bool) {
	entry, ok := c.byCiphertext.Load(ciphertext)
	if !ok {
		return "", false
	}
	return entry.(secretCacheEntry).plaintext, true
}

// BatchEncrypter is an extension of an Encrypter which supports processing secret encryption operations in batches.
// This is constructed by calling BatchingSecretsManager.BeginBatchEncryption.
type BatchEncrypter interface {
	config.Encrypter

	// Enqueue a secret for encryption at some point in the future. The ciphertext will be written to the target secret
	// object when the batch operation is processed.
	// This method is thread-safe and can be called concurrently by multiple goroutines.
	Enqueue(ctx context.Context, source *resource.Secret, plaintext string, target *apitype.SecretV1) error
}

// CompleteCrypterBatch is a function that must be called to ensure that all enqueued crypter operations are processed.
type CompleteCrypterBatch func(context.Context) error

type cachingBatchEncrypter struct {
	encrypter     config.Encrypter
	cache         SecretCache
	queue         chan queuedEncryption
	closed        atomic.Bool
	completeMutex sync.Mutex
	maxBatchSize  int
}

type queuedEncryption struct {
	source    *resource.Secret
	target    *apitype.SecretV1
	plaintext string
}

// DefaultMaxBatchEncryptCount is the default maximum number of items that can be enqueued for batch encryption.
const DefaultMaxBatchEncryptCount = 1000

// Ensure that cachingBatchEncrypter implements the BatchEncrypter interface for compatibility.
var _ BatchEncrypter = (*cachingBatchEncrypter)(nil)

// BeginBatchEncryptionWithCache returns a new BatchEncrypter and CompleteCrypterBatch function with a custom cache.
// The BatchEncrypter allows encryption operations to be batched to avoid round-trips for when the underlying encrypter
// requires a network call. Encryptions to a secret are enqueued and processed in a batch request either when the queue
// is full or when the wrapping transaction is committed. If the cache has an entry for a secret and the plaintext has
// not changed, the previous ciphertext is used immediately and not enqueued for the batch operation. Results are
// also written to the provided cache.
//
// The CompleteCrypterBatch function must be called to ensure that all enqueued encryption operations are processed.
//
//	batchEncrypter, completeCrypterBatch := BeginEncryptionBatch(encrypter)
//	SerializeSecrets(ctx, batchEncrypter, secrets)
//	err := completeCrypterBatch(ctx)
func BeginBatchEncryptionWithCache(
	encrypter config.Encrypter, cache SecretCache,
) (BatchEncrypter, CompleteCrypterBatch) {
	return beginBatchEncryption(encrypter, cache, DefaultMaxBatchEncryptCount)
}

func beginBatchEncryption(
	encrypter config.Encrypter, cache SecretCache, maxBatchSize int,
) (BatchEncrypter, CompleteCrypterBatch) {
	contract.Assertf(encrypter != nil, "encrypter must not be nil")
	contract.Assertf(cache != nil, "cache must not be nil")
	contract.Assertf(maxBatchSize > 0, "maxBatchSize must be greater than 0")
	batchEncrypter := &cachingBatchEncrypter{
		encrypter:    encrypter,
		cache:        cache,
		queue:        make(chan queuedEncryption, maxBatchSize),
		maxBatchSize: maxBatchSize,
	}
	return batchEncrypter, func(ctx context.Context) error {
		wasClosed := batchEncrypter.closed.Swap(true)
		contract.Assertf(!wasClosed, "batch encrypter already completed")
		return batchEncrypter.sendNextBatch(ctx)
	}
}

func (be *cachingBatchEncrypter) Enqueue(ctx context.Context,
	source *resource.Secret, plaintext string, target *apitype.SecretV1,
) error {
	contract.Assertf(source != nil, "source secret must not be nil")
	contract.Assertf(!be.closed.Load(), "batch encrypter must not be closed")
	// Add to the queue
	for {
		select {
		case be.queue <- queuedEncryption{source, target, plaintext}:
			return nil
		default:
			// If the queue is full, process the queue to make room.
			if err := be.sendNextBatch(ctx); err != nil {
				return err
			}
			// Now retry the enqueue.
		}
	}
}

// sendNextBatch processes any pending encryption operations in the queue.
// This method is thread-safe and can be called concurrently by multiple goroutines.
func (be *cachingBatchEncrypter) sendNextBatch(ctx context.Context) error {
	if len(be.queue) == 0 {
		return nil
	}
	// Only send 1 batch at a time
	be.completeMutex.Lock()
	defer be.completeMutex.Unlock()

	// Flush the encrypt queue
	dequeued := make([]queuedEncryption, 0, len(be.queue))
	plaintexts := make([]string, 0, len(be.queue))
	// Take up to the maximum number of items from the queue.
	// Other items might be enqueued concurrently and will be sent in the next batch.
dequeue:
	for range be.maxBatchSize {
		select {
		case q := <-be.queue:
			dequeued = append(dequeued, q)
			plaintexts = append(plaintexts, q.plaintext)
		default: // Queue is empty
			break dequeue
		}
	}

	ciphertexts := make([]string, len(dequeued))
	// If the cache has entries for all secrets, re-use the previous ciphertexts to save the re-encryption cost.
	cacheMissed := false
	for i, q := range dequeued {
		if ciphertext, ok := be.cache.LookupCiphertext(q.source, q.plaintext); ok {
			ciphertexts[i] = ciphertext
		} else {
			cacheMissed = true
			ciphertexts = nil
			break
		}
	}
	if cacheMissed {
		var err error
		ciphertexts, err = be.encrypter.BatchEncrypt(ctx, plaintexts)
		if err != nil {
			return err
		}
	}
	for i, q := range dequeued {
		ciphertext := ciphertexts[i]
		q.target.Ciphertext = ciphertext
		be.cache.Write(q.plaintext, ciphertext, q.source)
	}
	return nil
}

func (be *cachingBatchEncrypter) EncryptValue(ctx context.Context, plaintext string) (string, error) {
	return be.encrypter.EncryptValue(ctx, plaintext)
}

func (be *cachingBatchEncrypter) BatchEncrypt(ctx context.Context, plaintexts []string) ([]string, error) {
	return be.encrypter.BatchEncrypt(ctx, plaintexts)
}

// BatchDecrypter is an extension of a Decrypter which supports processing secret decryption operations in batches.
// This is constructed by calling BatchingSecretsManager.BeginBatchDecryption.
type BatchDecrypter interface {
	config.Decrypter

	// Enqueue a decryption operation to be completed. At some point in the future, the ciphertext will be decrypted and
	// the deserialized value will be written to the target secret object when the batch operation is processed.
	// This method is thread-safe and can be called concurrently by multiple goroutines.
	Enqueue(ctx context.Context, ciphertext string, target *resource.Secret) error
}

type cachingBatchDecrypter struct {
	decrypter                      config.Decrypter
	cache                          SecretCache
	deserializeSecretPropertyValue DeserializeSecretPropertyValue
	queue                          chan queuedDecryption
	closed                         atomic.Bool
	completeMutex                  sync.Mutex
	maxBatchSize                   int
}

type queuedDecryption struct {
	target     *resource.Secret
	ciphertext string
}

const DefaultMaxBatchDecryptCount = 1000

// Ensure that cachingBatchDecrypter implements the BatchDecrypter interface for compatibility.
var _ BatchDecrypter = (*cachingBatchDecrypter)(nil)

type DeserializeSecretPropertyValue func(plaintext string) (resource.PropertyValue, error)

// BeginDecryptionBatch returns a new BatchDecrypter and CompleteCrypterBatch function.
// The BatchDecrypter allows decryption operations to be batched to avoid round-trips for when the underlying decrypter
// requires a network call. Decryptions to a secret are enqueued and processed in a batch request either when the queue
// is full or when the wrapping transaction is committed.
//
// The CompleteCrypterBatch function must be called to ensure that all enqueued decryption operations are processed.
//
//	batchDecrypter, completeCrypterBatch := BeginDecryptionBatch(decrypter)
//	DeserializeSecrets(ctx, batchDecrypter, secrets)
//	err := completeCrypterBatch(ctx)
func BeginDecryptionBatch(decrypter config.Decrypter) (BatchDecrypter, CompleteCrypterBatch) {
	return BeginBatchDecryptionWithCache(decrypter, nullSecretCache{})
}

// BeginBatchDecryptionWithCache returns a new BatchDecrypter and CompleteCrypterBatch function with a custom cache.
// The BatchDecrypter allows decryption operations to be batched to avoid round-trips for when the underlying decrypter
// requires a network call. Decryptions to a secret are enqueued and processed in a batch request either when the queue
// is full or when the wrapping transaction is committed. If the cache has an entry for a ciphertext, the plaintext is
// used immediately and not enqueued for the batch operation. Results are also written to the provided cache.
//
// The CompleteCrypterBatch function must be called to ensure that all enqueued decryption operations are processed.
//
//	batchDecrypter, completeCrypterBatch := BeginDecryptionBatch(decrypter)
//	DeserializeSecrets(ctx, batchDecrypter, secrets)
//	err := completeCrypterBatch(ctx)
func BeginBatchDecryptionWithCache(
	decrypter config.Decrypter, cache SecretCache,
) (BatchDecrypter, CompleteCrypterBatch) {
	return beginBatchDecryption(decrypter, cache, secretPropertyValueFromPlaintext, DefaultMaxBatchDecryptCount)
}

func beginBatchDecryption(decrypter config.Decrypter, cache SecretCache,
	secretPropertyValueFromPlaintext DeserializeSecretPropertyValue, maxBatchSize int,
) (BatchDecrypter, CompleteCrypterBatch) {
	contract.Assertf(decrypter != nil, "decrypter must not be nil")
	contract.Assertf(cache != nil, "cache must not be nil")
	contract.Assertf(maxBatchSize > 0, "maxBatchSize must be greater than 0")
	batchDecrypter := &cachingBatchDecrypter{
		decrypter:                      decrypter,
		cache:                          cache,
		deserializeSecretPropertyValue: secretPropertyValueFromPlaintext,
		queue:                          make(chan queuedDecryption, maxBatchSize),
		maxBatchSize:                   maxBatchSize,
	}
	return batchDecrypter, func(ctx context.Context) error {
		wasClosed := batchDecrypter.closed.Swap(true)
		contract.Assertf(!wasClosed, "batch decrypter already completed")
		return batchDecrypter.sendNextBatch(ctx)
	}
}

func (bd *cachingBatchDecrypter) Enqueue(ctx context.Context, ciphertext string, target *resource.Secret) error {
	contract.Assertf(target != nil, "target secret must not be nil")
	contract.Assertf(!bd.closed.Load(), "batch decrypter must not be closed")
	// Add to the queue
	for {
		select {
		case bd.queue <- queuedDecryption{target, ciphertext}:
			return nil
		default:
			// If the queue is full, process the queue to make room.
			if err := bd.sendNextBatch(ctx); err != nil {
				return err
			}
			// Now retry the enqueue.
		}
	}
}

// sendNextBatch processes any pending decryption operations in the queue.
// This method is thread-safe and can be called concurrently by multiple goroutines.
func (bd *cachingBatchDecrypter) sendNextBatch(ctx context.Context) error {
	if len(bd.queue) == 0 {
		return nil
	}
	// Only send 1 batch at a time
	bd.completeMutex.Lock()
	defer bd.completeMutex.Unlock()

	// Flush the decrypt queue
	dequeued := make([]queuedDecryption, 0, len(bd.queue))
	ciphertexts := make([]string, 0, len(bd.queue))
	// Take up to the maximum number of items from the queue.
	// Other items might be enqueued concurrently and will be sent in the next batch.
dequeue:
	for range bd.maxBatchSize {
		select {
		case q := <-bd.queue:
			dequeued = append(dequeued, q)
			ciphertexts = append(ciphertexts, q.ciphertext)
		default: // Queue is empty
			break dequeue
		}
	}

	plaintexts := make([]string, len(dequeued))
	// If the cache has entries for all ciphertexts, re-use the previous plaintexts to save the re-decryption cost.
	cacheMissed := false
	for i, q := range dequeued {
		if plaintext, ok := bd.cache.LookupPlaintext(q.ciphertext); ok {
			plaintexts[i] = plaintext
		} else {
			cacheMissed = true
			plaintexts = nil
			break
		}
	}
	if cacheMissed {
		var err error
		plaintexts, err = bd.decrypter.BatchDecrypt(ctx, ciphertexts)
		if err != nil {
			return err
		}
	}
	for i, q := range dequeued {
		plaintext := plaintexts[i]
		propertyValue, err := bd.deserializeSecretPropertyValue(plaintext)
		if err != nil {
			return err
		}
		q.target.Element = propertyValue
		bd.cache.Write(plaintext, q.ciphertext, q.target)
	}
	return nil
}

func (bd *cachingBatchDecrypter) DecryptValue(ctx context.Context, ciphertext string) (string, error) {
	return bd.decrypter.DecryptValue(ctx, ciphertext)
}

func (bd *cachingBatchDecrypter) BatchDecrypt(ctx context.Context, ciphertexts []string) ([]string, error) {
	return bd.decrypter.BatchDecrypt(ctx, ciphertexts)
}
