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

// Bootstrap forms a new history group from this, the FIRST device. It registers
// self, mints the group's History Key, wraps it for our own device key, and
// activates self (the server pins HK version 1). After Bootstrap the HK is
// resolved in RAM and Push/PullOthers can run.
func (s *Syncer) Bootstrap(ctx context.Context, deviceName string) error {
	if _, err := s.registerSelf(ctx, deviceName); err != nil {
		return err
	}

	hk, err := cryptobox.NewHistoryKey()
	if err != nil {
		return fmt.Errorf("syncer: new history key: %w", err)
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

// Register enrolls this, a NON-first device, as pending and returns a short
// verification code derived from our public key. An existing active device
// confirms the same code (via VerificationCode over the pending device's
// PubKey) before approving, guarding against a swapped key.
func (s *Syncer) Register(ctx context.Context, deviceName string) (string, error) {
	dev, err := s.registerSelf(ctx, deviceName)
	if err != nil {
		return "", err
	}
	pub, err := cryptobox.PublicFromBytes(dev.PubKey)
	if err != nil {
		return "", fmt.Errorf("syncer: server returned malformed pubkey: %w", err)
	}
	return VerificationCode(pub), nil
}

// registerSelf posts this device's registration (idempotent-ish: a re-register
// surfaces the server's 409). It returns the server's device record.
func (s *Syncer) registerSelf(ctx context.Context, deviceName string) (wire.Device, error) {
	pub := s.dev.Public()
	return s.http.RegisterDevice(ctx, wire.RegisterReq{
		ID:     s.deviceID,
		Name:   deviceName,
		PubKey: pub[:],
	})
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
