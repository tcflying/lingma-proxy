//go:build windows

package qodercli

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

const (
	dpapiPrefix              = "DPAPI"
	cryptProtectUIForbidden  = 0x01
	uninstallRegistryBaseKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
)

var (
	crypt32       = syscall.NewLazyDLL("crypt32.dll")
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	procUnprotect = crypt32.NewProc("CryptUnprotectData")
	procLocalFree = kernel32.NewProc("LocalFree")
)

type cryptDataBlob struct {
	size uint32
	_    uint32
	data *byte
}

// loadAppCredential decrypts the Electron profile's OSCrypt-wrapped login state.
func loadAppCredential(dir string) (appCredential, error) {
	raw, err := os.ReadFile(filepath.Join(dir, authFile))
	if err != nil {
		return appCredential{}, fmt.Errorf("read %s: %w", authFile, err)
	}
	if len(raw) < 32 || !strings.HasPrefix(string(raw[:3]), "v10") {
		return appCredential{}, fmt.Errorf("%s: unexpected encryption envelope", authFile)
	}
	key, err := osCryptKey(dir)
	if err != nil {
		return appCredential{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return appCredential{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return appCredential{}, err
	}
	body := raw[3:]
	plaintext, err := gcm.Open(nil, body[:gcm.NonceSize()], body[gcm.NonceSize():], nil)
	if err != nil {
		return appCredential{}, fmt.Errorf("decrypt %s: %w", authFile, err)
	}
	var cred appCredential
	if err := json.Unmarshal(plaintext, &cred); err != nil {
		return appCredential{}, fmt.Errorf("parse %s: %w", authFile, err)
	}
	if !cred.valid() {
		return appCredential{}, fmt.Errorf("%s: no access token present", authFile)
	}
	return cred, nil
}

// osCryptKey unwraps the AES-256 key Chromium-style Local State stores, which
// is itself protected by the Windows user-level DPAPI master key.
func osCryptKey(dir string) ([]byte, error) {
	body, err := os.ReadFile(filepath.Join(dir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("read Local State: %w", err)
	}
	var state struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return nil, fmt.Errorf("parse Local State: %w", err)
	}
	if state.OSCrypt.EncryptedKey == "" {
		return nil, fmt.Errorf("Local State has no os_crypt.encrypted_key")
	}
	wrapped, err := base64.StdEncoding.DecodeString(state.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("decode Local State key: %w", err)
	}
	if len(wrapped) <= len(dpapiPrefix) {
		return nil, fmt.Errorf("Local State key is truncated")
	}
	return cryptUnprotect(wrapped[len(dpapiPrefix):])
}

func cryptUnprotect(in []byte) ([]byte, error) {
	inBlob := cryptDataBlob{size: uint32(len(in)), data: &in[0]}
	var outBlob cryptDataBlob
	ret, _, err := procUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0,
		0,
		0,
		0,
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed: %w", err)
	}
	if outBlob.data == nil || outBlob.size == 0 {
		return nil, fmt.Errorf("CryptUnprotectData returned an empty blob")
	}
	// The decrypted buffer belongs to DPAPI and must not outlive this call, so
	// copy it before freeing: returning unsafe.Slice of it would hand the caller
	// memory the allocator is about to reuse.
	out := make([]byte, outBlob.size)
	copy(out, unsafe.Slice(outBlob.data, outBlob.size))
	procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.data)))
	return out, nil
}

// registryInstallRoots reads the uninstall entries the desktop app writes so a
// non-default install directory is still discovered.
func registryInstallRoots() []string {
	keys := []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER}
	var roots []string
	for _, hive := range keys {
		for _, base := range []string{uninstallRegistryBaseKey, `WOW6432Node\` + uninstallRegistryBaseKey} {
			parent, err := registry.OpenKey(hive, base, registry.READ)
			if err != nil {
				continue
			}
			names, err := parent.ReadSubKeyNames(-1)
			parent.Close()
			if err != nil {
				continue
			}
			for _, name := range names {
				sub, err := registry.OpenKey(hive, base+`\`+name, registry.READ)
				if err != nil {
					continue
				}
				display, _, errDis := sub.GetStringValue("DisplayName")
				icon, _, errIcon := sub.GetStringValue("DisplayIcon")
				location, _, _ := sub.GetStringValue("InstallLocation")
				sub.Close()
				if errDis != nil || !strings.Contains(strings.ToLower(display), "qoder") {
					continue
				}
				if root := strings.TrimSpace(location); root != "" {
					roots = append(roots, root)
					continue
				}
				if errIcon == nil {
					roots = append(roots, filepath.Dir(strings.Trim(icon, `"`)))
				}
			}
		}
	}
	return roots
}
