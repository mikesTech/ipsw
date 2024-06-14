package aea

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/cloudflare/circl/hpke"
)

//go:embed data/fcs-keys.gz
var keyData []byte

type Header struct {
	Magic   [4]byte // AEA1
	Version uint32
	Length  uint32
}

type fcsResponse struct {
	EncRequest string `json:"enc-request,omitempty"`
	WrappedKey string `json:"wrapped-key,omitempty"`
}

type Keys map[string][]byte

func getKeys() (Keys, error) {
	var keys Keys

	zr, err := gzip.NewReader(bytes.NewReader(keyData))
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	if err := json.NewDecoder(zr).Decode(&keys); err != nil {
		return nil, fmt.Errorf("failed unmarshaling ipsw_db data: %w", err)
	}

	return keys, nil
}

type PrivateKey []byte

func (k PrivateKey) UnmarshalBinaryPrivateKey() ([]byte, error) {
	block, _ := pem.Decode(k)
	parsedKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse p8 key: %v", err)
	}
	pkey, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key must be of type ecdsa.PrivateKey")
	}
	return pkey.D.Bytes(), nil
}

type Metadata map[string][]byte

func (md Metadata) GetPrivateKey(data []byte) (map[string]PrivateKey, error) {
	out := make(map[string]PrivateKey)

	if len(data) > 0 {
		out["com.apple.wkms.fcs-key-url"] = PrivateKey(data)
		return out, nil
	}

	privKeyURL, ok := md["com.apple.wkms.fcs-key-url"]
	if !ok {
		return nil, fmt.Errorf("fcs-key-url key NOT found")
	}

	// check if keys are already loaded
	if keys, err := getKeys(); err == nil {
		u, err := url.Parse(string(privKeyURL))
		if err != nil {
			return nil, err
		}
		for k, v := range keys {
			if strings.EqualFold(k, path.Base(u.Path)) {
				out[k] = PrivateKey(v)
				return out, nil
			}
		}
	}

	resp, err := http.Get(string(privKeyURL))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	privKey, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(string(privKeyURL))
	if err != nil {
		return nil, err
	}
	out[path.Base(u.Path)] = PrivateKey(privKey)

	return out, nil
}

func (md Metadata) DecryptFCS(pemData []byte) (string, error) {
	ddata, ok := md["com.apple.wkms.fcs-response"]
	if !ok {
		return "", fmt.Errorf("no 'com.apple.wkms.fcs-response' found in AEA metadata")
	}
	var fcsResp fcsResponse
	if err := json.Unmarshal(ddata, &fcsResp); err != nil {
		return "", err
	}
	encRequestData, err := base64.StdEncoding.WithPadding(base64.StdPadding).DecodeString(fcsResp.EncRequest)
	if err != nil {
		return "", err
	}
	wrappedKeyData, err := base64.StdEncoding.WithPadding(base64.StdPadding).DecodeString(fcsResp.WrappedKey)
	if err != nil {
		return "", err
	}

	pkmap, err := md.GetPrivateKey(pemData)
	if err != nil {
		return "", err
	}
	var privKey []byte
	for _, pk := range pkmap {
		privKey, err = pk.UnmarshalBinaryPrivateKey()
		if err != nil {
			return "", err
		}
	}

	kemID := hpke.KEM_P256_HKDF_SHA256
	kdfID := hpke.KDF_HKDF_SHA256
	aeadID := hpke.AEAD_AES256GCM

	suite := hpke.NewSuite(kemID, kdfID, aeadID)

	privateKey, err := kemID.Scheme().UnmarshalBinaryPrivateKey(privKey)
	if err != nil {
		return "", err
	}
	recv, err := suite.NewReceiver(privateKey, nil)
	if err != nil {
		return "", err
	}
	opener, err := recv.Setup(encRequestData)
	if err != nil {
		return "", err
	}
	wkey, err := opener.Open(wrappedKeyData, nil)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(wkey), nil
}

func Info(in string) (Metadata, error) {
	var metadata Metadata
	f, err := os.Open(in)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var hdr Header
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil {
		return nil, err
	}

	if string(hdr.Magic[:]) != "AEA1" {
		return nil, fmt.Errorf("invalid AEA header: found '%s' expected 'AEA1'", string(hdr.Magic[:]))
	}

	metadata = make(map[string][]byte)
	mdr := io.NewSectionReader(f, int64(binary.Size(hdr)), int64(hdr.Length))

	// parse key-value pairs
	for {
		var length uint32
		err := binary.Read(mdr, binary.LittleEndian, &length)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		keyval := make([]byte, length-uint32(binary.Size(length)))
		if _, err = mdr.Read(keyval); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}

		k, v, _ := bytes.Cut(keyval, []byte{0x00})
		metadata[string(k)] = v
	}

	return metadata, nil
}

func Decrypt(in, out string, privKeyData []byte) (string, error) {
	metadata, err := Info(in)
	if err != nil {
		return "", fmt.Errorf("failed to parse AEA: %v", err)
	}

	wkey, err := metadata.DecryptFCS(privKeyData)
	if err != nil {
		return "", fmt.Errorf("failed to HPKE decrypt fcs-key: %v", err)
	}

	return aea(in, filepath.Join(out, filepath.Base(strings.TrimSuffix(in, filepath.Ext(in)))), wkey)
}

func aea(in, out, key string) (string, error) {
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("/usr/bin/aea", "decrypt", "-i", in, "-o", out, "-key-value", fmt.Sprintf("base64:%s", key))
		cout, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%v: %s", err, cout)
		}
		return out, nil
	}
	return "", fmt.Errorf("only supported on macOS (due to `aea` binary requirement)")
}