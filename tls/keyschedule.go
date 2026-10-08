// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tls

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

// TLS 1.3 的密钥派生（RFC 8446 section 7.1）。
//
// TLS 1.3 和 1.2 最大的不同就在这儿：1.2 的密钥来自一堆"PRF + 标签"
// 的拼凑，1.3 改成了一条**密钥计划**（key schedule）——从共享密钥出发，
// 一层层用 HKDF 派生，每层带一个标签，派生出握手密钥、应用密钥、
// 重密钥用的 key update 密钥。
//
// 这条链是：
//
//	PSK 或 0
//	  ↓ HKDF-Extract(salt=0, ikm=PSK)
//	Early Secret
//	  ↓ Derive-Secret("derived")
//	  ↓ HKDF-Extract(salt=derived, ikm=ECDHE 共享密钥)
//	Handshake Secret
//	  ├─→ 客户端握手流量密钥 / 服务端握手流量密钥
//	  ↓ Derive-Secret("derived")
//	  ↓ HKDF-Extract(salt=derived, ikm=0)
//	Master Secret
//	  ├─→ 客户端应用流量密钥 / 服务端应用流量密钥
//	  └─→ 导出密钥（exporter）、resumption 密钥
//
// 实现里用 **HKDF-SHA256**（TLS_AES_128_GCM_SHA256 那个套件）。
// SHA384 的套件（TLS_AES_256_GCM_SHA384）也要用 SHA384，那是另一套
// 参数——这里先做 SHA256 那套，它是最常用的。

var ErrKeySchedule = errors.New("tls: key schedule error")

// hkdfExtract 是 HKDF-Extract（RFC 5869 section 2.2）。
//
//	PRK = HMAC-Hash(salt, IKM)
func hkdfExtract(salt, ikm []byte) []byte {
	h := hmac.New(sha256.New, salt)
	h.Write(ikm)
	return h.Sum(nil)
}

// hkdfExpandLabel 是 HKDF-Expand-Label（RFC 8446 section 7.1）。
//
// TLS 1.3 的标签不是直接当 info 用，而是包一层固定格式的结构：
//
//	uint16 length
//	opaque label<7..255>    "tls13 " + 标签名
//	opaque context<0..255>
//
// 为什么要包：标签和上下文都要显式带长度，避免"两个不同的标签拼出来
// 一样"这种歧义。
func hkdfExpandLabel(secret []byte, label string, context []byte, length int) []byte {
	fullLabel := "tls13 " + label

	info := make([]byte, 0, 2+1+len(fullLabel)+1+len(context))
	info = append(info, byte(length>>8), byte(length))
	info = append(info, byte(len(fullLabel)))
	info = append(info, fullLabel...)
	info = append(info, byte(len(context)))
	info = append(info, context...)

	return hkdfExpand(secret, info, length)
}

// hkdfExpand 是 HKDF-Expand（RFC 5869 section 2.3）。
//
//	T(0) = empty
//	T(n) = HMAC-Hash(PRK, T(n-1) | info | n)
//	OKM  = 前 length 字节
func hkdfExpand(secret, info []byte, length int) []byte {
	var out []byte
	var t []byte
	for i := byte(1); len(out) < length; i++ {
		h := hmac.New(sha256.New, secret)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{i})
		t = h.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// deriveSecret 是 Derive-Secret（RFC 8446 section 7.1）。
//
//	Derive-Secret(Secret, Label, Messages)
//	  = HKDF-Expand-Label(Secret, Label, Transcript-Hash(Messages), Hash.length)
//
// 注意第三个参数是**消息的哈希**（transcript hash），不是消息本身——
// 这样密钥绑定的是整段握手的历史，任何一条消息被改动都会导致密钥不同。
func deriveSecret(secret []byte, label string, transcriptHash []byte) []byte {
	return hkdfExpandLabel(secret, label, transcriptHash, sha256.Size)
}

// keySchedule 是一条连接的密钥派生状态。
//
// 它随握手推进：每收到一条关键的握手消息，就把它的哈希喂进 transcript，
// 然后派生下一组密钥。
type keySchedule struct {
	// transcript 是到目前为止所有握手消息的累积哈希（RFC 8446 4.4.1）。
	//
	// **必须在每条消息的原文上算**，不是算完再拼——所以这里留了一个
	// sha256 的 running hash。顺序错了派生出的密钥就全错，症状是
	// "Finished 校验失败"，很难看出是哪儿的问题。
	transcript *transcriptHash

	// 各阶段的密钥（按 RFC 的链顺序）
	earlySecret     []byte
	handshakeSecret []byte
	masterSecret    []byte
}

func newKeySchedule() *keySchedule {
	return &keySchedule{transcript: newTranscriptHash()}
}

// deriveEarly 从 PSK（没有就是 0）派生 Early Secret。
//
//	Early Secret = HKDF-Extract(salt=0, IKM=PSK 或 0)
func (ks *keySchedule) deriveEarly(psk []byte) {
	if psk == nil {
		// 没配 PSK 时 IKM 是"一串 0"，长度是哈希长度（RFC 8446 7.1）
		psk = make([]byte, sha256.Size)
	}
	ks.earlySecret = hkdfExtract(make([]byte, sha256.Size), psk)
}

// deriveHandshake 从 ECDHE 共享密钥派生 Handshake Secret。
//
//	derived = Derive-Secret(Early Secret, "derived", "")
//	Handshake Secret = HKDF-Extract(salt=derived, IKM=ECDHE)
func (ks *keySchedule) deriveHandshake(sharedSecret []byte) error {
	if ks.earlySecret == nil {
		return errors.New("tls: handshake before early secret")
	}
	derived := deriveSecret(ks.earlySecret, "derived", sha256Sum(nil))
	ks.handshakeSecret = hkdfExtract(derived, sharedSecret)
	return nil
}

// deriveMaster 派生 Master Secret。
//
//	derived = Derive-Secret(Handshake Secret, "derived", "")
//	Master Secret = HKDF-Extract(salt=derived, IKM=0)
func (ks *keySchedule) deriveMaster() error {
	if ks.handshakeSecret == nil {
		return errors.New("tls: master before handshake secret")
	}
	derived := deriveSecret(ks.handshakeSecret, "derived", sha256Sum(nil))
	ks.masterSecret = hkdfExtract(derived, make([]byte, sha256.Size))
	return nil
}

// trafficKeys 是一组流量密钥（一个方向的）。
type trafficKeys struct {
	key []byte // AEAD 的密钥（16 字节，AES-128-GCM）
	iv  []byte // 记录的 nonce 基（12 字节）
}

// handshakeTrafficKeys 派生握手阶段的流量密钥。
//
//	client/server_handshake_traffic_secret =
//	    Derive-Secret(Handshake Secret, "c hs traffic" / "s hs traffic", CH..SH)
//	key = HKDF-Expand-Label(secret, "key", "", key_length)
//	iv  = HKDF-Expand-Label(secret, "iv", "", iv_length)
func (ks *keySchedule) handshakeTrafficKeys(isClient bool) trafficKeys {
	label := "c hs traffic"
	if !isClient {
		label = "s hs traffic"
	}
	secret := deriveSecret(ks.handshakeSecret, label, ks.transcript.sum())
	return trafficKeys{
		key: hkdfExpandLabel(secret, "key", nil, 16),
		iv:  hkdfExpandLabel(secret, "iv", nil, 12),
	}
}

// applicationTrafficKeys 派生应用阶段的流量密钥。
//
//	client/server_application_traffic_secret_0 =
//	    Derive-Secret(Master Secret, "c ap traffic" / "s ap traffic", CH..server Finished)
func (ks *keySchedule) applicationTrafficKeys(isClient bool) trafficKeys {
	label := "c ap traffic"
	if !isClient {
		label = "s ap traffic"
	}
	secret := deriveSecret(ks.masterSecret, label, ks.transcript.sum())
	return trafficKeys{
		key: hkdfExpandLabel(secret, "key", nil, 16),
		iv:  hkdfExpandLabel(secret, "iv", nil, 12),
	}
}

// keysFromSecret 从一个流量 secret 派生 AEAD 的 key 和 iv。
//
// 拆出来是因为 Finished 需要 secret 本身（不只是 AEAD），所以握手阶段
// 的 secret 要单独存一份。
func keysFromSecret(secret []byte) trafficKeys {
	return trafficKeys{
		key: hkdfExpandLabel(secret, "key", nil, 16),
		iv:  hkdfExpandLabel(secret, "iv", nil, 12),
	}
}

// finishedKey 派生校验 Finished 用的密钥。
//
//	finished_key = HKDF-Expand-Label(base_key, "finished", "", Hash.length)
//
// base_key 是 handshake traffic secret。
func finishedKey(baseSecret []byte) []byte {
	return hkdfExpandLabel(baseSecret, "finished", nil, sha256.Size)
}

// finishedVerifyData 算 Finished 消息里的校验数据。
//
//	verify_data = HMAC(finished_key, Transcript-Hash(握手到此刻))
//
// 它是整个握手的"指纹"：两边各自算一遍，对不上就说明中间有任何一条
// 消息被改了（或者顺序错了、或者密钥派生错了）。
func finishedVerifyData(baseSecret []byte, transcriptHash []byte) []byte {
	h := hmac.New(sha256.New, finishedKey(baseSecret))
	h.Write(transcriptHash)
	return h.Sum(nil)
}

// ---------------------------------------------------------------------------
// transcript 哈希

// transcriptHash 是握手的累积哈希。
//
// 为什么不直接存消息原文：TLS 1.3 要的是"到目前为止所有握手消息的
// SHA-256"，存原文在握手很长时太占内存（证书链可以很大）。累进哈希
// 只占 32 字节，而且**顺序天然正确**——按喂进去的顺序。
//
// 注意：这里不能直接传 transcriptHash 给 hmac 当消息用，因为 Finished
// 要的是"喂进去之后"的哈希，而规则要求 Finished 消息本身**不算**在
// 内（先算哈希，再把自己加进去）。
type transcriptHash struct {
	h hashWriter
}

func newTranscriptHash() *transcriptHash {
	return &transcriptHash{h: sha256.New()}
}

func (t *transcriptHash) write(b []byte) {
	t.h.Write(b)
}

// sum 返回当前的哈希值（不改变状态）。
func (t *transcriptHash) sum() []byte {
	return t.h.Sum(nil)
}

// sha256Sum 算一段数据的 SHA-256（小工具）。
func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// hashWriter 是 hash.Hash 的最小接口（省得导入 hash 包）。
type hashWriter interface {
	Write(p []byte) (int, error)
	Sum(b []byte) []byte
}
