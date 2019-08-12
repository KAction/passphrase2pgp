package openpgp

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"hash"
	"io"
	"time"

	"golang.org/x/crypto/ed25519"
)

const (
	// SignKeyPubLen is the size of the public part of an OpenPGP packet.
	SignKeyPubLen = 53
	signKeySecLen = 3 + 32 + 2

	// FlagMDC indicates that the identity making a self-signature
	// prefers to recieve a Modification Detection Code (MDC).
	FlagMDC = iota

	// Encoded S2K octet count.
	s2kCount = 0xff // maximum strength
)

var (
	// DecryptKeyErr indicates the wrong key was given.
	DecryptKeyErr = errors.New("wrong encryption key")

	// DecryptKeyErr indicates the wrong key was given.
	UnsupportedPacketErr = errors.New("input packet unsupported")
)

// SignKey represents an Ed25519 sign key (EdDSA).
type SignKey struct {
	Key     ed25519.PrivateKey
	created int64
	expires int64
	packet  []byte
}

// Seed sets the 32-byte seed for a sign key.
func (k *SignKey) Seed(seed []byte) {
	k.Key = ed25519.NewKeyFromSeed(seed)
	k.packet = nil
}

// Created returns the key's creation date in unix epoch seconds.
func (k *SignKey) Created() int64 {
	return k.created
}

// SetCreated sets the creation date in unix epoch seconds.
func (k *SignKey) SetCreated(time int64) {
	k.created = time
	k.packet = nil
}

// Expired returns the key's expiration time in unix epoch seconds. A
// value of zero means the key doesn't expire.
func (k *EncryptKey) Expires() int64 {
	return k.expires
}

// SetExpire returns the key's expiration time in unix epoch seconds. A
// value of zero means the key doesn't expire.
func (k *EncryptKey) SetExpires(time int64) {
	k.expires = time
}

// Load key material from packet body. If the error is DecryptKeyErr,
// then either the passphrase was nil or the passphrase is wrong. To use
// an empty passphrase, pass an empty but non-nil passphrase.
func (k *SignKey) Load(packet Packet, passphrase []byte) (err error) {
	defer func() {
		if recover() != nil {
			err = InvalidPacketErr
		}
	}()

	switch packet.Tag {
	case 5:
		// Ok
	case 6:
		// TODO: Support loading public key packets
		return UnsupportedPacketErr
	default:
		// Wrong packet type
		return InvalidPacketErr
	}

	// Check various static bytes
	body := packet.Body
	if body[0] != 0x04 || !bytes.Equal(body[5:19], []byte{
		22, 9,
		0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01,
		0x01, 0x07, 0x40,
	}) {
		return UnsupportedPacketErr
	}

	pubkey := body[19:51]
	var seckey []byte
	created := int64(binary.BigEndian.Uint32(body[1:]))
	k.SetCreated(created)

	if body[51] == 0 {
		// Unencrypted
		var tail []byte
		seckey, tail = mpiDecode(body[52:], 32)
		if len(tail) != 2 {
			return InvalidPacketErr
		}
	} else if body[51] == 254 {
		// Encrypted
		if passphrase == nil {
			return DecryptKeyErr // missing passphrase
		}

		if body[52] != 9 || // AES-256
			body[53] != 3 || // Iterated and Salted S2K
			body[54] != 8 { // SHA-256
			return UnsupportedPacketErr
		}
		salt := body[55:63]
		count := decodeS2K(body[63])
		iv := body[64:80]
		data := body[80:]
		key := s2k(passphrase, salt, count)

		block, _ := aes.NewCipher(key)
		stream := cipher.NewCFBDecrypter(block, iv)
		stream.XORKeyStream(data, data)
		var check []byte
		seckey, check = mpiDecode(data, 32)
		if seckey == nil {
			return DecryptKeyErr
		}

		mac := sha1.New()
		mac.Write(mpi(seckey))
		if subtle.ConstantTimeCompare(mac.Sum(nil), check) == 0 {
			return DecryptKeyErr
		}
	}

	k.Seed(seckey)
	if !bytes.Equal(k.Pubkey(), pubkey) {
		return InvalidPacketErr
	}
	return nil
}

// Seckey returns the public key part of a sign key.
func (k *SignKey) Seckey() []byte {
	return k.Key[:32]
}

// Pubkey returns the public key part of a sign key.
func (k *SignKey) Pubkey() []byte {
	return k.Key[32:]
}

// Packet returns an OpenPGP packet for a sign key.
func (k *SignKey) Packet() []byte {
	be := binary.BigEndian

	if k.packet != nil {
		return k.packet
	}

	packet := make([]byte, SignKeyPubLen+1, SignKeyPubLen+signKeySecLen)
	packet[0] = 0xc0 | 5 // packet header, Secret-Key Packet (5)
	packet[2] = 0x04     // packet version, new (4)

	// Public Key
	be.PutUint32(packet[3:], uint32(k.created)) // creation date
	packet[7] = 22                              // algorithm, EdDSA
	packet[8] = 9                               // OID length
	// OID (1.3.6.1.4.1.11591.15.1)
	oid := []byte{0x2b, 0x06, 0x01, 0x04, 0x01, 0xda, 0x47, 0x0f, 0x01}
	copy(packet[9:], oid)
	be.PutUint16(packet[18:], 263)  // public key length (always 263 bits)
	packet[20] = 0x40               // MPI prefix
	copy(packet[21:53], k.Pubkey()) // public key (32 bytes)

	// Secret Key
	packet[53] = 0 // string-to-key, unencrypted
	mpikey := mpi(k.Seckey())
	packet = append(packet, mpikey...)
	// Append checksum
	packet = packet[:len(packet)+2]
	be.PutUint16(packet[len(packet)-2:], checksum(mpikey))

	packet[1] = byte(len(packet) - 2) // packet length
	k.packet = packet
	return packet
}

// PubPacket returns a public key packet for this key.
func (k *SignKey) PubPacket() []byte {
	packet := make([]byte, SignKeyPubLen)
	packet[0] = 0xc0 | 6 // packet header, Public-Key packet (6)
	packet[1] = SignKeyPubLen - 2
	copy(packet[2:], k.Packet()[2:])
	return packet
}

func decodeS2K(c byte) int {
	return (16 + int(c&15)) << (uint(c>>4) + 6)
}

// Compute a symmetric protection key via S2K.
func s2k(passphrase, salt []byte, count int) []byte {
	h := sha256.New()
	// Note: This implements S2K as it is actually used in practice by
	// both GnuPG and PGP. The OpenPGP standard (3.7.1.3) is subtly
	// incorrect in its description, and that algorithm is not used by
	// actual implementations.
	// https://dev.gnupg.org/T4676
	full := make([]byte, 8+len(passphrase))
	copy(full[0:], salt)
	copy(full[8:], passphrase)
	iterations := count / len(full)
	for i := 0; i < iterations; i++ {
		h.Write(full)
	}
	tail := count - iterations*len(full)
	h.Write(full[:tail])
	return h.Sum(nil)
}

func (k *SignKey) EncPacket(passphrase []byte) []byte {
	var saltIV [24]byte
	if _, err := rand.Read(saltIV[:]); err != nil {
		panic(err) // should never happen
	}
	salt := saltIV[:8]
	iv := saltIV[8:]

	// Compute symmetric protection key via S2K
	key := s2k(passphrase, salt, decodeS2K(s2kCount))

	// Encrypt the secret key, with SHA-1 "MAC"
	mpikey := mpi(k.Seckey())
	mac := sha1.New()
	mac.Write(mpikey)
	seckey := mac.Sum(mpikey)
	block, _ := aes.NewCipher(key)
	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(seckey, seckey)

	// Rewrite secret portion of packet
	packet := k.Packet()[:57]
	packet[53] = 254 // encrypted with S2K
	packet[54] = 9   // AES-256
	packet[55] = 3   // Iterated and Salted S2K
	packet[56] = 8   // SHA-256
	packet = append(packet, salt...)
	packet = append(packet, s2kCount)
	packet = append(packet, iv...)
	packet = append(packet, seckey...)
	packet[1] = byte(len(packet) - 2) // update packet length
	return packet
}

// KeyID returns the Key ID for a sign key.
func (k *SignKey) KeyID() []byte {
	h := sha1.New()
	h.Write([]byte{0x99, 0, 51})         // "packet" length = 51
	h.Write(k.Packet()[2:SignKeyPubLen]) // public key portion
	return h.Sum(nil)
}

type subpacket struct {
	Type byte
	Data []byte
}

// Bind a subkey to this signing key, returning the signature packet.
func (k *SignKey) Bind(subkey *EncryptKey, when int64) []byte {
	const sigtype = 0x18 // Subkey Binding Signature
	h := sha256.New()
	pubkey := k.PubPacket()
	h.Write([]byte{0x99, 0, byte(len(pubkey) - 2)})
	h.Write(pubkey[2:])
	pubsubkey := subkey.PubPacket()
	h.Write([]byte{0x99, 0, byte(len(pubsubkey) - 2)})
	h.Write(pubsubkey[2:])

	subpackets := []subpacket{
		// Key Flags subpacket (encrypt)
		{Type: 27, Data: []byte{0x0c}},
	}
	if subkey.expires != 0 {
		// Key Expiration Time packet
		delta := uint32(subkey.expires - subkey.created)
		expires := subpacket{Type: 9, Data: marshal32be(delta)}
		subpackets = append(subpackets, expires)
	}

	return k.sign(sigInput{h, sigtype, when, subpackets})
}

func (k *SignKey) SelfSign(userid *UserID, when int64, flags int) []byte {
	const sigtype = 0x13 // Positive certification
	h := sha256.New()
	key := k.PubPacket()
	h.Write([]byte{0x99, 0, byte(len(key) - 2)})
	h.Write(key[2:])
	uid := userid.Packet()
	h.Write([]byte{0xb4, 0, 0, 0, byte(len(uid) - 2)})
	h.Write(uid[2:])

	// An Issuer Fingerprint subpacket is unnecessary here because this
	// is a self-signature, and so even the Issuer subpacket is already
	// redundant. The recipient already knows which key we're talking
	// about. Technically the Issuer subpacket is optional, but GnuPG
	// will not import a key without it.
	var subpackets []subpacket

	// Key Flags subpacket (type=27) [sign and certify]
	// This is necessary since some implementations (GitHub) treat
	// all flags as if they were zero if not present.
	keyflags := subpacket{
		Type: 27,
		Data: []byte{0x03},
	}
	subpackets = append(subpackets, keyflags)

	if k.expires != 0 {
		// Key Expiration Time subpacket (type=9)
		expires := subpacket{
			Type: 9,
			Data: marshal32be(uint32(k.expires - k.created)),
		}
		subpackets = append(subpackets, expires)
	}

	if flags&FlagMDC != 0 {
		// Features subpacket (type=30)
		mdc := subpacket{Type: 30, Data: []byte{0x01}}
		subpackets = append(subpackets, mdc)
	}

	return k.sign(sigInput{h, sigtype, when, subpackets})
}

// Certify a pairing of public key and user ID packet, returning the
// signature packet. This accept byte slices so that arbitrary packets
// can be certified, not just formats understood by this package.
func (k *SignKey) Certify(key, uid []byte, when int64) []byte {
	const sigtype = 0x10 // Generic certification
	h := sha256.New()

	prefix := []byte{0x99, 0, 0}
	keypkt, _, _ := ParsePacket(key)
	binary.BigEndian.PutUint16(prefix[1:], uint16(len(keypkt.Body)))
	h.Write(prefix)
	h.Write(keypkt.Body)

	prefix = []byte{0xb4, 0, 0, 0, 0}
	uidpkt, _, _ := ParsePacket(uid)
	binary.BigEndian.PutUint32(prefix[1:], uint32(len(uidpkt.Body)))
	h.Write(prefix)
	h.Write(uidpkt.Body)

	subpackets := []subpacket{fingerprint(k.KeyID())}
	return k.sign(sigInput{h, sigtype, when, subpackets})
}

// Sign binary data with this key using an OpenPGP signature packet.
func (k *SignKey) Sign(src io.Reader) ([]byte, error) {
	const sigtype = 0x00 // Binary document
	// Compute digest to be signed
	h := sha256.New()
	if _, err := io.Copy(h, src); err != nil {
		return nil, err
	}
	subpackets := []subpacket{fingerprint(k.KeyID())}
	in := sigInput{h, sigtype, time.Now().Unix(), subpackets}
	return k.sign(in), nil
}

// Clearsign returns a new cleartext stream signer. Data from the
// given reader will be cleartext-signed and wrtten into the returned
// reader. The returned reader must either be read completely or closed.
func (k *SignKey) Clearsign(src io.Reader) io.ReadCloser {
	const sigtype = 0x01 // Text document
	r, w := io.Pipe()
	go func() {
		open := []byte("-----BEGIN PGP SIGNED MESSAGE-----\nHash: SHA256\n\n")
		crlf := []byte("\r\n")
		tmp := make([]byte, 128)
		if _, err := w.Write(open); err != nil {
			return
		}
		s := bufio.NewScanner(src)
		h := sha256.New()
		first := true
		for s.Scan() {
			line := s.Bytes()

			// Remove trailing whitespace
			for i := len(line) - 1; i >= 0; i-- {
				if line[i] == 0x20 || line[i] == 0x09 {
					line = line[:i]
				} else {
					break
				}
			}

			// Append to hash
			if !first {
				h.Write(crlf)
			}
			first = false
			h.Write(line)

			// Pass through dash-encoded
			if len(line) > 0 && line[0] == 0x2d {
				tmp = tmp[:2]
				tmp[0] = 0x2d
				tmp[1] = 0x20
			} else {
				tmp = tmp[:0]
			}
			tmp = append(tmp, line...)
			tmp = append(tmp, 0x0a)
			if _, err := w.Write(tmp); err != nil {
				return
			}
		}
		if err := s.Err(); err != nil {
			w.CloseWithError(err)
		}

		subpackets := []subpacket{fingerprint(k.KeyID())}
		in := sigInput{h, sigtype, time.Now().Unix(), subpackets}
		sig := Armor(k.sign(in))
		if _, err := w.Write(sig); err != nil {
			return
		}
		w.Close()
	}()
	return r
}

func fingerprint(keyid []byte) subpacket {
	// Issuer Fingerprint subpacket (length=22, type=33)
	return subpacket{Type: 33, Data: append([]byte{0x04}, keyid...)}
}

type sigInput struct {
	h          hash.Hash
	sigtype    byte
	when       int64
	subpackets []subpacket
}

func (k *SignKey) sign(in sigInput) []byte {
	var subpackets []subpacket

	packet := make([]byte, 8, 257)
	packet[0] = 0xc0 | 2   // packet header, new format, Signature Packet (2)
	packet[2] = 0x04       // packet version, new (4)
	packet[3] = in.sigtype // signature type
	packet[4] = 22         // public-key algorithm, EdDSA
	packet[5] = 8          // hash algorithm, SHA-256

	// Signature Creation Time subpacket (type=2)
	sigCreated := subpacket{
		Type: 2,
		Data: marshal32be(uint32(in.when)),
	}
	subpackets = append(subpackets, sigCreated)

	// Issuer subpacket (type=16)
	issuer := subpacket{
		Type: 16,
		Data: k.KeyID()[12:20],
	}
	subpackets = append(subpackets, issuer)

	subpackets = append(subpackets, in.subpackets...)
	for _, subpacket := range subpackets {
		packet = append(packet, byte(len(subpacket.Data)+1))
		packet = append(packet, subpacket.Type)
		packet = append(packet, subpacket.Data...)
	}

	// Hashed subpacket data length
	hashedLen := uint16(len(packet) - 8)
	binary.BigEndian.PutUint16(packet[6:8], hashedLen)

	// Unhashed subpacket data (none)
	packet = packet[:len(packet)+2]
	binary.BigEndian.PutUint16(packet[len(packet)-2:], 0)

	// Write hash trailers
	h := in.h
	h.Write(packet[2 : hashedLen+8])                       // trailer
	h.Write([]byte{4, 0xff, 0, 0, 0, byte(hashedLen + 6)}) // final trailer

	// Compute hash and sign
	sigsum := h.Sum(nil)
	sig := ed25519.Sign(k.Key, sigsum)

	// hash preview
	packet = append(packet, sigsum[:2]...)

	// signature
	r := sig[:32]
	packet = append(packet, mpi(r)...)
	m := sig[32:]
	packet = append(packet, mpi(m)...)

	// Finalize
	packet[1] = byte(len(packet)) - 2 // packet length
	return packet
}
