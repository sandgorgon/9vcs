package patches

// BlobStore is a content-addressed store of whole-file content, for paths
// that go through KindBlob changes instead of the line graph — see the
// ChangeKind doc comment. Same on-disk shape as Store, different payload:
// raw file bytes instead of an encoded Patch.
type BlobStore struct {
	raw *rawStore
}

// OpenBlobs returns a BlobStore rooted at dir, creating it if necessary.
func OpenBlobs(dir string) (*BlobStore, error) {
	raw, err := openRaw(dir)
	if err != nil {
		return nil, err
	}
	return &BlobStore{raw: raw}, nil
}

// Put stores data, returning its content hash. Idempotent.
func (s *BlobStore) Put(data []byte) (Hash, error) { return s.raw.put(data) }

// Get retrieves the blob with hash h.
func (s *BlobStore) Get(h Hash) ([]byte, error) { return s.raw.get(h) }

// Has reports whether h is present in the store.
func (s *BlobStore) Has(h Hash) bool { return s.raw.has(h) }
