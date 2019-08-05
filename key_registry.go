/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dgraph-io/badger/pb"
	"github.com/dgraph-io/badger/y"
)

const (
	// KeyRegistryFileName is the file name for the key registry file.
	KeyRegistryFileName = "KEYREGISTRY"
	// KeyRegistryRewriteFileName is the file name for the rewrite key registry file.
	KeyRegistryRewriteFileName = "REWRITE-KEYREGISTRY"
	// RotationPeriod is the key rotation period for datakey.
	RotationPeriod = 10 * 24 * time.Hour
)

// SanityText is used to check whether the given user provided storage key is valid or not
var sanityText = []byte("Hello Badger")

// KeyRegistry used to maintain all the data keys.
type KeyRegistry struct {
	sync.RWMutex
	dataKeys      map[uint64]*pb.DataKey
	lastCreated   int64 //lastCreated is the timestamp of the last data key generated.
	nextKeyID     uint64
	encryptionKey []byte
	fp            *os.File
}

func newKeyRegistry(storageKey []byte) *KeyRegistry {
	return &KeyRegistry{
		dataKeys:      make(map[uint64]*pb.DataKey),
		nextKeyID:     0,
		encryptionKey: storageKey,
	}
}

// OpenKeyRegistry opens key registry if it exists, otherwise it'll create key registry
// and returns key registry.
func OpenKeyRegistry(opt Options) (*KeyRegistry, error) {
	path := filepath.Join(opt.Dir, KeyRegistryFileName)
	var flags uint32
	if opt.ReadOnly {
		flags |= y.ReadOnly
	}
	fp, err := y.OpenExistingFile(path, flags)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// Creating new registry file if not exist.
		kr := newKeyRegistry(opt.EncryptionKey)
		if opt.ReadOnly {
			return kr, nil
		}
		// Writing the key regitry to the file.
		if err := WriteKeyRegistry(kr, opt); err != nil {
			return nil, err
		}
		fp, err = y.OpenExistingFile(path, flags)
		if err != nil {
			return nil, err
		}
	}
	kr, err := readKeyRegistry(fp, opt.EncryptionKey)
	if err != nil {
		// This case happens only if the file is opened properly and
		// not able to read.
		fp.Close()
		return nil, err
	}
	// We are seeking the end because, we don't incremental read.
	// In readKeyRegistry we use ReadAt.
	_, err = kr.fp.Seek(0, io.SeekEnd)
	if err != nil {
		fp.Close()
		return nil, err
	}
	return kr, nil
}

func readKeyRegistry(fp *os.File, encryptionKey []byte) (*KeyRegistry, error) {
	readPos := int64(0)
	// Read the IV.
	iv, err := y.ReadAt(fp, readPos, aes.BlockSize)
	if err != nil {
		return nil, err
	}
	readPos += aes.BlockSize
	// Read sanity text.
	eSanityText, err := y.ReadAt(fp, readPos, len(sanityText))
	if err != nil {
		return nil, err
	}
	if len(encryptionKey) > 0 {
		var err error
		// Decrpting sanity text.
		eSanityText, err = y.XORBlock(eSanityText, encryptionKey, iv)
		if err != nil {
			return nil, err
		}
	}
	// Check the given key is valid or not.
	if !bytes.Equal(eSanityText, sanityText) {
		return nil, ErrEncryptionKeyMismatch
	}
	readPos += int64(len(sanityText))
	stat, err := fp.Stat()
	if err != nil {
		return nil, err
	}
	kr := newKeyRegistry(encryptionKey)
	for {
		// Read all the datakey till the file ends.
		if readPos == stat.Size() {
			break
		}
		// Reading crc and crc length.
		lenCrcBuf, err := y.ReadAt(fp, readPos, 8)
		if err != nil {
			return nil, err
		}
		readPos += 8
		l := int64(binary.BigEndian.Uint32(lenCrcBuf[0:4]))
		data, err := y.ReadAt(fp, readPos, int(l))
		if err != nil {
			return nil, err
		}
		if crc32.Checksum(data, y.CastagnoliCrcTable) != binary.BigEndian.Uint32(lenCrcBuf[4:]) {
			return nil, errBadChecksum
		}
		dataKey := &pb.DataKey{}
		if err = dataKey.Unmarshal(data); err != nil {
			return nil, err
		}
		if len(encryptionKey) > 0 {
			// Decrypt the key if the storage key exits.
			if dataKey.Data, err = y.XORBlock(dataKey.Data, encryptionKey, dataKey.Iv); err != nil {
				return nil, err
			}
		}
		if dataKey.KeyId > kr.nextKeyID {
			// Set the maximum key ID for next key ID generation.
			kr.nextKeyID = dataKey.KeyId
		}
		if dataKey.CreatedAt > kr.lastCreated {
			// Set the last generated key timestamp.
			kr.lastCreated = dataKey.CreatedAt
		}
		// No need to lock, since we building the initial state.
		kr.dataKeys[kr.nextKeyID] = dataKey
		readPos += l
	}
	kr.fp = fp
	return kr, nil
}

// WriteKeyRegistry will rewrite the existing key registry file with new one
func WriteKeyRegistry(reg *KeyRegistry, opt Options) error {
	tmpPath := filepath.Join(opt.Dir, KeyRegistryRewriteFileName)
	// Open temporary file to write the data and do atomic rename.
	fp, err := y.OpenTruncFile(tmpPath, false)
	if err != nil {
		return err
	}
	buf := &bytes.Buffer{}
	iv, err := y.GenereateIV()
	if err != nil {
		return err
	}

	// Encrypt sanity text if the storage presents.
	eSanity := sanityText
	if len(opt.EncryptionKey) > 0 {
		var err error
		eSanity, err = y.XORBlock(eSanity, opt.EncryptionKey, iv)
		if err != nil {
			return err
		}
	}
	if _, err = buf.Write(iv); err != nil {
		fp.Close()
		return err
	}
	if _, err = buf.Write(eSanity); err != nil {
		fp.Close()
		return err
	}

	// Write all the datakeys to the disk.
	for _, k := range reg.dataKeys {
		// Wrting the datakey to the given file fd.
		if err := storeDataKey(buf, opt.EncryptionKey, k, false); err != nil {
			fp.Close()
			return err
		}
	}

	// Write buf to the disk.
	if _, err = fp.Write(buf.Bytes()); err != nil {
		fp.Close()
		return err
	}

	// Sync the file.
	if err = y.FileSync(fp); err != nil {
		fp.Close()
		return err
	}
	registryPath := filepath.Join(opt.Dir, KeyRegistryFileName)
	if err = fp.Close(); err != nil {
		return err
	}
	// Rename to the original file.
	if err = os.Rename(tmpPath, registryPath); err != nil {
		return err
	}
	return syncDir(opt.Dir)
}

func (kr *KeyRegistry) dataKey(id uint64) (*pb.DataKey, error) {
	if id == 0 {
		return nil, nil
	}
	dk, ok := kr.dataKeys[id]
	if !ok {
		return nil, ErrInvalidDataKeyID
	}
	return dk, nil
}

func (kr *KeyRegistry) latestDataKey() (*pb.DataKey, error) {
	if len(kr.encryptionKey) == 0 {
		return nil, nil
	}

	// Time diffrence from the last generated time.
	diff := time.Since(time.Unix(kr.lastCreated, 0))
	if diff < RotationPeriod {
		// If less than 10 days, returns the last generaterd key.
		kr.RLock()
		defer kr.RUnlock()
		dk := kr.dataKeys[kr.nextKeyID]
		return dk, nil
	}

	// Otherwise Increment the KeyID and generate new datakey
	kr.nextKeyID++
	k := make([]byte, len(kr.encryptionKey))
	iv, err := y.GenereateIV()
	if err != nil {
		return nil, err
	}
	_, err = rand.Read(k)
	if err != nil {
		return nil, err
	}
	dk := &pb.DataKey{
		KeyId:     kr.nextKeyID,
		Data:      k,
		CreatedAt: time.Now().Unix(),
		Iv:        iv,
	}
	// Store the datekey.
	buf := &bytes.Buffer{}
	err = storeDataKey(buf, kr.encryptionKey, dk, true)
	if err != nil {
		return nil, err
	}
	// Persist the datakey to the disk
	if _, err = kr.fp.Write(buf.Bytes()); err != nil {
		return nil, err
	}
	if err = y.FileSync(kr.fp); err != nil {
		return nil, err
	}
	// storeDatakey encrypts the datakey So, placing unencrypted key in the memory
	dk.Data = k
	kr.Lock()
	defer kr.Unlock()
	kr.lastCreated = dk.CreatedAt
	kr.dataKeys[kr.nextKeyID] = dk
	return dk, nil
}

// Close closes the key registry.
func (kr *KeyRegistry) Close() error {
	return kr.fp.Close()
}

func storeDataKey(buf *bytes.Buffer, storageKey []byte, k *pb.DataKey, sync bool) error {
	if len(storageKey) > 0 {
		var err error
		// In memory, we'll have decrypted key.
		if k.Data, err = y.XORBlock(k.Data, storageKey, k.Iv); err != nil {
			return err
		}
	}
	data, err := k.Marshal()
	if err != nil {
		return err
	}
	var lenCrcBuf [8]byte
	binary.BigEndian.PutUint32(lenCrcBuf[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(lenCrcBuf[4:8], crc32.Checksum(data, y.CastagnoliCrcTable))
	if _, err = buf.Write(lenCrcBuf[:]); err != nil {
		return err
	}
	if _, err = buf.Write(data); err != nil {
		return err
	}
	return nil
}