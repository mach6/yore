package syncer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"yore/internal/cryptobox"
	"yore/internal/wire"
)

// bootstrapHKVersion is the HK generation created when the first device forms
// the group. The server accepts any version >= 1 at bootstrap and pins it.
const bootstrapHKVersion = 1

// Enroll registers this machine using a single-use enrollment token and
// reports which path it landed on.
//
// The server decides: with no active device in the group this machine forms it
// (Bootstrap), otherwise it is pending and an enrolled machine must approve the
// returned verification code. The newcomer cannot determine this itself — it is
// not yet authorized to read anything — so the registration response carries it.
func (s *Syncer) Enroll(ctx context.Context, deviceName, token string) (formed bool, code string, err error) {
	pub := s.dev.Public()
	resp, err := s.http.RegisterDevice(ctx, wire.RegisterReq{
		ID:      s.deviceID,
		Name:    deviceName,
		PubKey:  pub[:],
		SignKey: s.dev.SignPublic(),
	}, token)
	if err != nil {
		return false, "", err
	}
	devPub, err := cryptobox.PublicFromBytes(resp.Device.PubKey)
	if err != nil {
		return false, "", fmt.Errorf("syncer: server returned malformed pubkey: %w", err)
	}
	return resp.GroupFormed, VerificationCode(devPub), nil
}

// Bootstrap forms a new history group from this, the FIRST device: it mints the
// group's History Key, wraps it for our own device key, and activates self (the
// server pins HK version 1). The device must already be registered (see Enroll).
//
// It also installs the recovery key, and does so BEFORE self-activation: if
// recovery cannot be established the group must not come into existence at all,
// because the only moment a recovery wrap can be created is while the HK is
// held in RAM by its creator.
func (s *Syncer) Bootstrap(ctx context.Context, recovery cryptobox.RecoveryKey, salt []byte) error {
	hk, err := cryptobox.NewHistoryKey()
	if err != nil {
		return fmt.Errorf("syncer: new history key: %w", err)
	}

	recPub := recovery.Public()
	recBlob, err := cryptobox.WrapHK(hk, recPub)
	if err != nil {
		return fmt.Errorf("syncer: wrap history key for recovery: %w", err)
	}
	if err := s.http.InitRecovery(ctx, wire.RecoveryInit{
		Salt:    salt,
		PubKey:  recPub[:],
		SignKey: recovery.SignPublic(),
		Wrap: wire.HKWrap{
			DeviceID:  RecoveryDeviceID,
			Blob:      recBlob,
			HKVersion: bootstrapHKVersion,
		},
	}); err != nil {
		return fmt.Errorf("syncer: install recovery key: %w", err)
	}

	pub := s.dev.Public()
	blob, err := cryptobox.WrapHK(hk, pub)
	if err != nil {
		return fmt.Errorf("syncer: wrap history key for self: %w", err)
	}
	req := wire.ActivateReq{Wrap: wire.HKWrap{
		DeviceID:  s.deviceID,
		Blob:      blob,
		HKVersion: bootstrapHKVersion,
	}}
	if err := s.http.ActivateDevice(ctx, s.deviceID, req); err != nil {
		return err
	}
	s.setHK(hk, bootstrapHKVersion)
	return nil
}

// MintToken issues a single-use enrollment token for adding another machine.
func (s *Syncer) MintToken(ctx context.Context) (wire.TokenResp, error) {
	return s.http.MintToken(ctx)
}

// RecoverHK retrieves the History Key using only the recovery passphrase, for
// when no enrolled device survives. It fetches the salt, derives the recovery
// keypair, proves possession by signing with it, and unwraps HK.
//
// It returns the History Key and the HK version the wrap carried. The key is
// NOT yet usable for sync: this machine still has to enroll and be admitted,
// which the caller drives — and after this call the passed client signs as the
// recovery identity, which is what authorizes both.
func RecoverHK(ctx context.Context, http *HTTPClient, phrase string) (hk [32]byte, hkVersion int, err error) {
	saltResp, err := http.RecoverySalt(ctx)
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("syncer: fetch recovery salt: %w", err)
	}
	rk, err := cryptobox.DeriveRecoveryKey(phrase, saltResp.Salt)
	if err != nil {
		return [32]byte{}, 0, err
	}

	// Sign as the recovery identity: there is no device record to sign as.
	http.SetSigner(RecoveryDeviceID, rk.Sign)
	wrap, err := http.RecoveryWrap(ctx)
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("syncer: fetch recovery wrap (wrong phrase?): %w", err)
	}
	hk, err = cryptobox.UnwrapHK(wrap.Blob, rk.DeviceKeyFor())
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("syncer: unwrap history key with the recovery phrase: %w", err)
	}
	return hk, wrap.HKVersion, nil
}

// AdoptHK installs a History Key obtained out of band (by RecoverHK) as this
// syncer's key, so the caller can wrap it for a freshly enrolled device.
func (s *Syncer) AdoptHK(hk [32]byte, version int) { s.setHK(hk, version) }

// ActivateWith wraps the currently-held HK for deviceID and activates it. It is
// how a recovered machine admits itself once it has the HK but no approver.
func (s *Syncer) ActivateWith(ctx context.Context, deviceID string, pub [32]byte) error {
	s.mu.Lock()
	hk, version := s.hk, s.hkVersion
	resolved := s.hkResolved
	s.mu.Unlock()
	if !resolved {
		return fmt.Errorf("syncer: no history key held")
	}
	blob, err := cryptobox.WrapHK(hk, pub)
	if err != nil {
		return fmt.Errorf("syncer: wrap history key for %s: %w", deviceID, err)
	}
	return s.http.ActivateDevice(ctx, deviceID, wire.ActivateReq{Wrap: wire.HKWrap{
		DeviceID:  deviceID,
		Blob:      blob,
		HKVersion: version,
	}})
}

// PendingDevices lists devices awaiting approval.
func (s *Syncer) PendingDevices(ctx context.Context) ([]wire.Device, error) {
	devs, err := s.http.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	var pending []wire.Device
	for _, d := range devs {
		if d.Status == wire.DevicePending {
			pending = append(pending, d)
		}
	}
	return pending, nil
}

// Devices returns every enrolled device, any status (for display).
func (s *Syncer) Devices(ctx context.Context) ([]wire.Device, error) {
	return s.http.ListDevices(ctx)
}

// Approve admits a pending device: this (active) device wraps the group's HK
// for the target's public key and activates it. The target can then resolve the
// same HK. The caller should first confirm the target's VerificationCode
// out-of-band.
func (s *Syncer) Approve(ctx context.Context, deviceID string) error {
	hk, version, err := s.resolveHK(ctx)
	if err != nil {
		return err
	}
	target, err := s.findDevice(ctx, deviceID)
	if err != nil {
		return err
	}
	pub, err := cryptobox.PublicFromBytes(target.PubKey)
	if err != nil {
		return fmt.Errorf("syncer: target %s has malformed pubkey: %w", deviceID, err)
	}
	blob, err := cryptobox.WrapHK(hk, pub)
	if err != nil {
		return fmt.Errorf("syncer: wrap history key for %s: %w", deviceID, err)
	}
	req := wire.ActivateReq{Wrap: wire.HKWrap{
		DeviceID:  deviceID,
		Blob:      blob,
		HKVersion: version,
	}}
	return s.http.ActivateDevice(ctx, deviceID, req)
}

// Revoke removes a device from the group and rotates the group key so the
// revoked device can decrypt nothing new. It revokes the device, then in one
// atomic Rotate: mints a fresh HK, re-wraps EVERY existing DEK under the new HK
// (preserving each DEK's plaintext, so old records stay decryptable without
// being re-encrypted), and wraps the new HK for every still-active device. This
// is the O(1) revocation property: only keys rotate, never the record corpus.
func (s *Syncer) Revoke(ctx context.Context, deviceID string) error {
	hkOld, versionOld, err := s.resolveHK(ctx)
	if err != nil {
		return err
	}
	if err := s.http.RevokeDevice(ctx, deviceID); err != nil {
		return err
	}

	hkNew, err := cryptobox.NewHistoryKey()
	if err != nil {
		return fmt.Errorf("syncer: new history key: %w", err)
	}
	newVersion := versionOld + 1

	// Re-wrap every DEK under the new HK, preserving each wrap's identity
	// (keyID, deviceID, epoch) so records still open under the same AAD.
	dekWraps, err := s.reWrapAllDEKs(ctx, hkOld, hkNew, versionOld, newVersion)
	if err != nil {
		return err
	}

	// Wrap the new HK for every still-active device (the revoked one is now
	// non-active and is excluded server-side and here).
	devs, err := s.http.ListDevices(ctx)
	if err != nil {
		return err
	}
	var hkWraps []wire.HKWrap
	for _, d := range devs {
		if d.Status != wire.DeviceActive {
			continue
		}
		pub, err := cryptobox.PublicFromBytes(d.PubKey)
		if err != nil {
			return fmt.Errorf("syncer: active device %s has malformed pubkey: %w", d.ID, err)
		}
		blob, err := cryptobox.WrapHK(hkNew, pub)
		if err != nil {
			return fmt.Errorf("syncer: wrap new history key for %s: %w", d.ID, err)
		}
		hkWraps = append(hkWraps, wire.HKWrap{DeviceID: d.ID, Blob: blob, HKVersion: newVersion})
	}
	if len(hkWraps) == 0 {
		return fmt.Errorf("syncer: refusing to rotate with no surviving active devices")
	}

	if err := s.http.Rotate(ctx, wire.RotateReq{
		HKVersion: newVersion,
		HKWraps:   hkWraps,
		DEKWraps:  dekWraps,
	}); err != nil {
		return err
	}

	s.setHK(hkNew, newVersion)
	return nil
}

// reWrapAllDEKs unwraps every DEK under the old HK and re-wraps it under the new
// HK, returning the full re-wrapped set the server's Rotate demands (it must
// cover exactly the existing keyIDs). Each wrap keeps its keyID, deviceID, and
// epoch, so the DEK plaintext — and therefore every record it sealed — is
// unchanged.
func (s *Syncer) reWrapAllDEKs(ctx context.Context, hkOld, hkNew [32]byte, oldVersion, newVersion int) ([]wire.DEKWrap, error) {
	var out []wire.DEKWrap
	cursor := ""
	for {
		resp, err := s.http.ListDEKWraps(ctx, cursor, dekPageLimit)
		if err != nil {
			return nil, err
		}
		for _, w := range resp.Wraps {
			dek, err := cryptobox.UnwrapDEK(w.Blob, hkOld, w.KeyID, w.DeviceID, w.Epoch, oldVersion)
			if err != nil {
				return nil, fmt.Errorf("syncer: unwrap DEK %s during rotation: %w", w.KeyID, err)
			}
			blob, err := cryptobox.WrapDEK(dek, hkNew, w.KeyID, w.DeviceID, w.Epoch, newVersion)
			if err != nil {
				return nil, fmt.Errorf("syncer: re-wrap DEK %s during rotation: %w", w.KeyID, err)
			}
			out = append(out, wire.DEKWrap{
				KeyID:     w.KeyID,
				DeviceID:  w.DeviceID,
				Epoch:     w.Epoch,
				Blob:      blob,
				HKVersion: newVersion,
			})
		}
		if resp.NextCursor == "" {
			return out, nil
		}
		cursor = resp.NextCursor
	}
}

// findDevice fetches one device record by ID from the device list.
func (s *Syncer) findDevice(ctx context.Context, deviceID string) (wire.Device, error) {
	devs, err := s.http.ListDevices(ctx)
	if err != nil {
		return wire.Device{}, err
	}
	for _, d := range devs {
		if d.ID == deviceID {
			return d, nil
		}
	}
	return wire.Device{}, fmt.Errorf("syncer: device %s not found", deviceID)
}

// VerificationCode derives a stable, human-comparable code from a device public
// key: six groups of four uppercase base32 characters over a domain-separated
// SHA-256 of the key. Both machines display it during enrollment so the
// approver can confirm the pending device is the intended one and not a
// substituted key.
func VerificationCode(pub [32]byte) string {
	sum := sha256.Sum256(append([]byte("yore/verify/v1|"), pub[:]...))
	const groups, per = 6, 4
	var b strings.Builder
	for g := 0; g < groups; g++ {
		if g > 0 {
			b.WriteByte('-')
		}
		for i := 0; i < per; i++ {
			b.WriteByte(base32Alphabet[sum[g*per+i]%32])
		}
	}
	return b.String()
}

// base32Alphabet is Crockford-style (no I, L, O, U) for unambiguous reading.
const base32Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
