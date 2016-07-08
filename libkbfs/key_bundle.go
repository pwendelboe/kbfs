// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/keybase/go-codec/codec"
)

// All section references below are to https://keybase.io/blog/kbfs-crypto
// (version 1.3).

// TODO once TLFKeyBundle is removed, ensure that methods take
// value receivers unless they mutate the receiver.

// TLFCryptKeyServerHalfID is the identifier type for a server-side key half.
type TLFCryptKeyServerHalfID struct {
	ID HMAC // Exported for serialization.
}

// String implements the Stringer interface for TLFCryptKeyServerHalfID.
func (id TLFCryptKeyServerHalfID) String() string {
	return id.ID.String()
}

// TLFCryptKeyInfo is a per-device key half entry in the
// TLFWriterKeyBundle/TLFReaderKeyBundle.
type TLFCryptKeyInfo struct {
	ClientHalf   IFCERFTEncryptedTLFCryptKeyClientHalf
	ServerHalfID TLFCryptKeyServerHalfID
	EPubKeyIndex int `codec:"i,omitempty"`

	codec.UnknownFieldSetHandler
}

type copyFields int

const (
	allFields copyFields = iota
	knownFieldsOnly
)

// DeviceKeyInfoMap is a map from a user devices (identified by the
// KID of the corresponding device CryptPublicKey) to the
// TLF's symmetric secret key information.
type DeviceKeyInfoMap map[keybase1.KID]TLFCryptKeyInfo

func (kim DeviceKeyInfoMap) fillInDeviceInfo(crypto IFCERFTCrypto, uid keybase1.UID, tlfCryptKey IFCERFTTLFCryptKey, ePrivKey TLFEphemeralPrivateKey, ePubIndex int,
	publicKeys []IFCERFTCryptPublicKey) (
	serverMap map[keybase1.KID]TLFCryptKeyServerHalf, err error) {
	serverMap = make(map[keybase1.KID]TLFCryptKeyServerHalf)
	// for each device:
	//    * create a new random server half
	//    * mask it with the key to get the client half
	//    * encrypt the client half
	//
	// TODO: parallelize
	for _, k := range publicKeys {
		// Skip existing entries, only fill in new ones
		if _, ok := kim[k.kid]; ok {
			continue
		}

		var serverHalf TLFCryptKeyServerHalf
		serverHalf, err = crypto.MakeRandomTLFCryptKeyServerHalf()
		if err != nil {
			return nil, err
		}

		var clientHalf TLFCryptKeyClientHalf
		clientHalf, err = crypto.MaskTLFCryptKey(serverHalf, tlfCryptKey)
		if err != nil {
			return nil, err
		}

		var encryptedClientHalf IFCERFTEncryptedTLFCryptKeyClientHalf
		encryptedClientHalf, err =
			crypto.EncryptTLFCryptKeyClientHalf(ePrivKey, k, clientHalf)
		if err != nil {
			return nil, err
		}

		var serverHalfID TLFCryptKeyServerHalfID
		serverHalfID, err =
			crypto.GetTLFCryptKeyServerHalfID(uid, k.kid, serverHalf)
		if err != nil {
			return nil, err
		}

		kim[k.kid] = TLFCryptKeyInfo{
			ClientHalf:   encryptedClientHalf,
			ServerHalfID: serverHalfID,
			EPubKeyIndex: ePubIndex,
		}
		serverMap[k.kid] = serverHalf
	}

	return serverMap, nil
}

// GetKIDs returns the KIDs for the given bundle.
func (kim DeviceKeyInfoMap) GetKIDs() []keybase1.KID {
	var keys []keybase1.KID
	for k := range kim {
		keys = append(keys, k)
	}
	return keys
}

// UserDeviceKeyInfoMap maps a user's keybase UID to their DeviceKeyInfoMap
type UserDeviceKeyInfoMap map[keybase1.UID]DeviceKeyInfoMap

// TLFWriterKeyBundle is a bundle of all the writer keys for a top-level
// folder.
type TLFWriterKeyBundle struct {
	// Maps from each writer to their crypt key bundle.
	WKeys UserDeviceKeyInfoMap

	// M_f as described in 4.1.1 of https://keybase.io/blog/kbfs-crypto.
	TLFPublicKey TLFPublicKey `codec:"pubKey"`

	// M_e as described in 4.1.1 of https://keybase.io/blog/kbfs-crypto.
	// Because devices can be added into the key generation after it
	// is initially created (so those devices can get access to
	// existing data), we track multiple ephemeral public keys; the
	// one used by a particular device is specified by EPubKeyIndex in
	// its TLFCryptoKeyInfo struct.
	TLFEphemeralPublicKeys IFCERFTTLFEphemeralPublicKeys `codec:"ePubKey"`

	codec.UnknownFieldSetHandler
}

// IsWriter returns true if the given user device is in the writer set.
func (tkb TLFWriterKeyBundle) IsWriter(user keybase1.UID, deviceKID keybase1.KID) bool {
	_, ok := tkb.WKeys[user][deviceKID]
	return ok
}

// TLFWriterKeyGenerations stores a slice of TLFWriterKeyBundle,
// where the last element is the current generation.
type TLFWriterKeyGenerations []TLFWriterKeyBundle

// LatestKeyGeneration returns the current key generation for this TLF.
func (tkg TLFWriterKeyGenerations) LatestKeyGeneration() IFCERFTKeyGen {
	return IFCERFTKeyGen(len(tkg))
}

// IsWriter returns whether or not the user+device is an authorized writer
// for the latest generation.
func (tkg TLFWriterKeyGenerations) IsWriter(user keybase1.UID, deviceKID keybase1.KID) bool {
	keyGen := tkg.LatestKeyGeneration()
	if keyGen < 1 {
		return false
	}
	return tkg[keyGen-1].IsWriter(user, deviceKID)
}

// TLFReaderKeyBundle stores all the user keys with reader
// permissions on a TLF
type TLFReaderKeyBundle struct {
	RKeys UserDeviceKeyInfoMap

	// M_e as described in 4.1.1 of https://keybase.io/blog/kbfs-crypto.
	// Because devices can be added into the key generation after it
	// is initially created (so those devices can get access to
	// existing data), we track multiple ephemeral public keys; the
	// one used by a particular device is specified by EPubKeyIndex in
	// its TLFCryptoKeyInfo struct.
	// This list is needed so a reader rekey doesn't modify the writer
	// metadata.
	TLFReaderEphemeralPublicKeys IFCERFTTLFEphemeralPublicKeys `codec:"readerEPubKey,omitempty"`

	codec.UnknownFieldSetHandler
}

// IsReader returns true if the given user device is in the reader set.
func (trb TLFReaderKeyBundle) IsReader(user keybase1.UID, deviceKID keybase1.KID) bool {
	_, ok := trb.RKeys[user][deviceKID]
	return ok
}

// TLFReaderKeyGenerations stores a slice of TLFReaderKeyBundle,
// where the last element is the current generation.
type TLFReaderKeyGenerations []TLFReaderKeyBundle

// LatestKeyGeneration returns the current key generation for this TLF.
func (tkg TLFReaderKeyGenerations) LatestKeyGeneration() IFCERFTKeyGen {
	return IFCERFTKeyGen(len(tkg))
}

// IsReader returns whether or not the user+device is an authorized reader
// for the latest generation.
func (tkg TLFReaderKeyGenerations) IsReader(user keybase1.UID, deviceKID keybase1.KID) bool {
	keyGen := tkg.LatestKeyGeneration()
	if keyGen < 1 {
		return false
	}
	return tkg[keyGen-1].IsReader(user, deviceKID)
}

type serverKeyMap map[keybase1.UID]map[keybase1.KID]TLFCryptKeyServerHalf

func fillInDevicesAndServerMap(crypto IFCERFTCrypto, newIndex int,
	cryptKeys map[keybase1.UID][]IFCERFTCryptPublicKey, keyInfoMap UserDeviceKeyInfoMap,
	ePubKey IFCERFTTLFEphemeralPublicKey, ePrivKey TLFEphemeralPrivateKey,
	tlfCryptKey IFCERFTTLFCryptKey, newServerKeys serverKeyMap) error {
	for u, keys := range cryptKeys {
		if _, ok := keyInfoMap[u]; !ok {
			keyInfoMap[u] = DeviceKeyInfoMap{}
		}

		serverMap, err := keyInfoMap[u].fillInDeviceInfo(
			crypto, u, tlfCryptKey, ePrivKey, newIndex, keys)
		if err != nil {
			return err
		}
		if len(serverMap) > 0 {
			newServerKeys[u] = serverMap
		}
	}
	return nil
}

// fillInDevices ensures that every device for every writer and reader
// in the provided lists has complete TLF crypt key info, and uses the
// new ephemeral key pair to generate the info if it doesn't yet
// exist.
func fillInDevices(crypto IFCERFTCrypto, wkb *TLFWriterKeyBundle, rkb *TLFReaderKeyBundle,
	wKeys map[keybase1.UID][]IFCERFTCryptPublicKey, rKeys map[keybase1.UID][]IFCERFTCryptPublicKey, ePubKey IFCERFTTLFEphemeralPublicKey, ePrivKey TLFEphemeralPrivateKey, tlfCryptKey IFCERFTTLFCryptKey) (
	serverKeyMap, error) {
	var newIndex int
	if len(wKeys) == 0 {
		// This is VERY ugly, but we need it in order to avoid having to
		// version the metadata. The index will be strictly negative for reader
		// ephemeral public keys
		rkb.TLFReaderEphemeralPublicKeys =
			append(rkb.TLFReaderEphemeralPublicKeys, ePubKey)
		newIndex = -len(rkb.TLFReaderEphemeralPublicKeys)
	} else {
		wkb.TLFEphemeralPublicKeys =
			append(wkb.TLFEphemeralPublicKeys, ePubKey)
		newIndex = len(wkb.TLFEphemeralPublicKeys) - 1
	}

	// now fill in the secret keys as needed
	newServerKeys := serverKeyMap{}
	err := fillInDevicesAndServerMap(crypto, newIndex, wKeys, wkb.WKeys,
		ePubKey, ePrivKey, tlfCryptKey, newServerKeys)
	if err != nil {
		return nil, err
	}
	err = fillInDevicesAndServerMap(crypto, newIndex, rKeys, rkb.RKeys,
		ePubKey, ePrivKey, tlfCryptKey, newServerKeys)
	if err != nil {
		return nil, err
	}
	return newServerKeys, nil
}
