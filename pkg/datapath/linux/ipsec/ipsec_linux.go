// Copyright 2019 Authors of Cilium
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
//
// +build linux

package ipsec

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/cilium/pkg/datapath/linux/linux_defaults"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/vishvananda/netlink"

	"github.com/sirupsen/logrus"
)

type IPSecDir string

const (
	IPSecDirIn   IPSecDir = "IPSEC_IN"
	IPSecDirOut  IPSecDir = "IPSEC_OUT"
	IPSecDirBoth IPSecDir = "IPSEC_BOTH"
)

type ipSecKey struct {
	Spi   uint8
	ReqID int
	Auth  *netlink.XfrmStateAlgo
	Crypt *netlink.XfrmStateAlgo
}

// ipSecKeysGlobal is safe to read unlocked because the only writers are from
// daemon init time before any readers will be online.
var ipSecKeysGlobal = make(map[string]*ipSecKey)

func getIPSecKeys(ip net.IP) *ipSecKey {
	key, scoped := ipSecKeysGlobal[ip.String()]
	if scoped == false {
		key, _ = ipSecKeysGlobal[""]
	}
	return key
}

func ipSecNewState() *netlink.XfrmState {
	state := netlink.XfrmState{
		Mode:  netlink.XFRM_MODE_TUNNEL,
		Proto: netlink.XFRM_PROTO_ESP,
		ESN:   false,
	}
	return &state
}

func ipSecNewPolicy() *netlink.XfrmPolicy {
	policy := netlink.XfrmPolicy{}
	return &policy
}

func ipSecAttachPolicyTempl(policy *netlink.XfrmPolicy, keys *ipSecKey, srcIP, dstIP net.IP) {
	tmpl := netlink.XfrmPolicyTmpl{
		Proto: netlink.XFRM_PROTO_ESP,
		Mode:  netlink.XFRM_MODE_TUNNEL,
		Spi:   int(keys.Spi),
		Reqid: keys.ReqID,
		Dst:   dstIP,
		Src:   srcIP,
	}

	policy.Tmpls = append(policy.Tmpls, tmpl)
}

func ipSecJoinState(state *netlink.XfrmState, keys *ipSecKey) {
	state.Auth = keys.Auth
	state.Crypt = keys.Crypt
	state.Spi = int(keys.Spi)
	state.Reqid = keys.ReqID
}

func ipSecReplaceState(remoteIP, localIP net.IP) (uint8, error) {
	key := getIPSecKeys(localIP)
	if key == nil {
		return 0, fmt.Errorf("IPSec key missing")
	}
	state := ipSecNewState()
	ipSecJoinState(state, key)
	state.Src = localIP
	state.Dst = remoteIP
	return key.Spi, netlink.XfrmStateAdd(state)
}

func ipSecReplacePolicyIn(src, dst *net.IPNet) error {
	if err := ipSecReplacePolicyInFwd(src, dst, netlink.XFRM_DIR_IN); err != nil {
		if !os.IsExist(err) {
			return err
		}
	}
	return ipSecReplacePolicyInFwd(src, dst, netlink.XFRM_DIR_FWD)
}

func ipSecReplacePolicyInFwd(src, dst *net.IPNet, dir netlink.Dir) error {
	var spiWide uint32

	key := getIPSecKeys(dst.IP)
	if key == nil {
		return fmt.Errorf("IPSec key missing")
	}
	spiWide = uint32(key.Spi)

	policy := ipSecNewPolicy()
	policy.Dir = dir
	policy.Src = src
	policy.Dst = dst
	policy.Mark = &netlink.XfrmMark{
		Value: ((spiWide << 12) | linux_defaults.RouteMarkDecrypt),
		Mask:  linux_defaults.IPsecMarkMask,
	}
	ipSecAttachPolicyTempl(policy, key, src.IP, dst.IP)
	return netlink.XfrmPolicyUpdate(policy)
}

func ipSecReplacePolicyOut(src, dst *net.IPNet, dir IPSecDir) error {
	var spiWide uint32

	key := getIPSecKeys(dst.IP)
	if key == nil {
		return fmt.Errorf("IPSec key missing")
	}
	spiWide = uint32(key.Spi)

	policy := ipSecNewPolicy()
	policy.Dir = netlink.XFRM_DIR_OUT
	policy.Src = src
	policy.Dst = dst
	policy.Mark = &netlink.XfrmMark{
		Value: ((spiWide << 12) | linux_defaults.RouteMarkEncrypt),
		Mask:  linux_defaults.IPsecMarkMask,
	}
	ipSecAttachPolicyTempl(policy, key, src.IP, dst.IP)
	return netlink.XfrmPolicyUpdate(policy)
}

func ipSecDeleteStateOut(src, local net.IP) error {
	state := ipSecNewState()

	state.Src = src
	state.Dst = local
	err := netlink.XfrmStateDel(state)
	return err
}

func ipSecDeleteStateIn(src, local net.IP) error {
	state := ipSecNewState()

	state.Src = src
	state.Dst = local
	err := netlink.XfrmStateDel(state)
	return err
}

func ipSecDeletePolicy(src, local net.IP) error {
	return nil
}

func ipsecDeleteXfrmSpi(spi uint8) {
	var err error
	scopedLog := log.WithFields(logrus.Fields{
		"spi": spi,
	})

	xfrmPolicyList, err := netlink.XfrmPolicyList(0)
	if err != nil {
		scopedLog.WithError(err).Warning("deleting previous SPI, xfrm policy list error")
		return
	}

	for _, p := range xfrmPolicyList {
		if p.Tmpls[0].Spi != int(spi) &&
			((p.Mark != nil && (p.Mark.Value&linux_defaults.RouteMarkMask) == linux_defaults.RouteMarkDecrypt) ||
				(p.Mark != nil && (p.Mark.Value&linux_defaults.RouteMarkMask) == linux_defaults.RouteMarkEncrypt)) {
			if err := netlink.XfrmPolicyDel(&p); err != nil {
				scopedLog.WithError(err).Warning("deleting old xfrm policy failed")
			}
		}
	}
	xfrmStateList, err := netlink.XfrmStateList(0)
	if err != nil {
		scopedLog.WithError(err).Warning("deleting previous SPI, xfrm state list error")
		return
	}
	for _, s := range xfrmStateList {
		if s.Spi != int(spi) {
			if err := netlink.XfrmStateDel(&s); err != nil {
				scopedLog.WithError(err).Warning("deleting old xfrm state failed")
			}
		}
	}
}

/* UpsertIPsecEndpoint updates the IPSec context for a new endpoint inserted in
 * the ipcache. Currently we support a global crypt/auth keyset that will encrypt
 * all traffic between endpoints. An IPSec context consists of two pieces a policy
 * and a state, the security policy database (SPD) and security association
 * database (SAD). These are implemented using the Linux kernels XFRM implementation.
 *
 * For all traffic that matches a policy, the policy tuple used is
 * (sip/mask, dip/mask, dev) with an optional mark field used in the Cilium implementation
 * to ensure only expected traffic is encrypted. The state hashtable is searched for
 * a matching state associated with that flow. The Linux kernel will do a series of
 * hash lookups to find the most specific state (xfrm_dst) possible. The hash keys searched are
 * the following, (daddr, saddr, reqid, encap_family), (daddr, wildcard, reqid, encap),
 * (mark, daddr, spi, proto, encap). Any "hits" in the hash table will subsequently
 * have the SPI checked to ensure it also matches. Encap is ignored in our case here
 * and can be used with UDP encap if wanted.
 *
 * The implications of the (inflexible!) hash key implementation is that in-order
 * to have a policy/state match we _must_ insert a state for each daddr. For Cilium
 * this translates to a state entry per node. We learn the nodes/endpoints by
 * listening to ipcache events. Finally, because IPSec is unidirectional a state
 * is needed for both ingress and egress. Denoted by the DIR on the xfrm cmd line
 * in the policy lookup. In the Cilium case, where we have IPSec between all
 * endpoints this results in two policy rules per node, one for ingress
 * and one for egress.
 *
 * For a concrete example consider two cluster nodes using transparent mode e.g.
 * without an IPSec tunnel IP. Cluster Node A has host_ip 10.156.0.1 with an
 * endpoint assigned to IP 10.156.2.2 and cluster Node B has host_ip 10.182.0.1
 * with an endpoint using IP 10.182.3.3. Then on Node A there will be a two policy
 * entries and a set of State entries,
 *
 * Policy1(src=10.182.0.0/16,dst=10.156.0.1/16,dir=in,tmpl(spi=#spi,reqid=#reqid))
 * Policy2(src=10.156.0.0/16,dst=10.182.0.1/16,dir=out,tmpl(spi=#spi,reqid=#reqid))
 * State1(src=*,dst=10.182.0.1,spi=#spi,reqid=#reqid,...)
 * State2(src=*,dst=10.156.0.1,spi=#spi,reqid=#reqid,...)
 *
 * Design Note: For newer kernels a BPF xfrm interface would greatly simplify the
 * state space. Basic idea would be to reference a state using any key generated
 * from BPF program allowing for a single state per security ctx.
 */
func UpsertIPsecEndpoint(local, remote *net.IPNet, dir IPSecDir) (uint8, error) {
	var spi uint8
	var err error

	/* TODO: state reference ID is (dip,spi) which can be duplicated in the current global
	 * mode. The duplication is on _all_ ingress states because dst_ip == host_ip in this
	 * case and only a single spi entry is in use. Currently no check is done to avoid
	 * attempting to add duplicate (dip,spi) states and we get 'file exist' error. These
	 * errors are expected at the moment but perhaps it would be better to avoid calling
	 * netlink API at all when we "know" an entry is a duplicate. To do this the xfer
	 * state would need to be cached in the ipcache.
	 */
	/* The two states plus policy below is sufficient for tunnel mode for
	 * transparent mode ciliumIP == nil case must also be handled.
	 */
	if !local.IP.Equal(remote.IP) {
		if dir == IPSecDirIn || dir == IPSecDirBoth {
			if spi, err = ipSecReplaceState(local.IP, remote.IP); err != nil {
				if !os.IsExist(err) {
					return 0, fmt.Errorf("unable to replace local state: %s", err)
				}
			}
			if err = ipSecReplacePolicyIn(remote, local); err != nil {
				if !os.IsExist(err) {
					return 0, fmt.Errorf("unable to replace policy in: %s", err)
				}
			}
		}

		if dir == IPSecDirOut || dir == IPSecDirBoth {
			if spi, err = ipSecReplaceState(remote.IP, local.IP); err != nil {
				if !os.IsExist(err) {
					return 0, fmt.Errorf("unable to replace remote state: %s", err)
				}
			}

			if err = ipSecReplacePolicyOut(local, remote, dir); err != nil {
				if !os.IsExist(err) {
					return 0, fmt.Errorf("unable to replace policy out: %s", err)
				}
			}
		}
	}
	return spi, nil
}

// DeleteIPSecEndpoint deletes the endpoint from IPSec SPD and SAD
func DeleteIPSecEndpoint(src, local net.IP) error {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.IPAddr: src,
	})

	err := ipSecDeleteStateIn(src, local)
	if err != nil {
		scopedLog.WithError(err).Warning("unable to delete IPSec (stateIn) context\n")
	}
	err = ipSecDeleteStateOut(src, local)
	if err != nil {
		scopedLog.WithError(err).Warning("unable to delete IPSec (stateOut) context\n")
	}
	err = ipSecDeletePolicy(src, local)
	if err != nil {
		scopedLog.WithError(err).Warning("unable to delete IPSec (policy) context\n")
	}
	return nil
}

func decodeIPSecKey(keyRaw string) ([]byte, error) {
	// As we have released the v1.4.0 docs telling the users to write the
	// k8s secret with the prefix "0x" we have to remove it if it is present,
	// so we can decode the secret.
	keyTrimmed := strings.TrimPrefix(keyRaw, "0x")
	return hex.DecodeString(keyTrimmed)
}

// LoadIPSecKeysFile imports IPSec auth and crypt keys from a file. The format
// is to put a key per line as follows, (auth-algo auth-key enc-algo enc-key)
func LoadIPSecKeysFile(path string) (uint8, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	return loadIPSecKeys(file)
}

func loadIPSecKeys(r io.Reader) (uint8, error) {
	var spi uint8
	scopedLog := log.WithFields(logrus.Fields{
		"spi": spi,
	})

	scanner := bufio.NewScanner(r)
	scanner.Split(bufio.ScanLines)
	for scanner.Scan() {
		var oldSpi uint8
		offset := 0

		ipSecKey := &ipSecKey{
			ReqID: 1,
		}

		// Scanning IPsec keys formatted as follows,
		//    auth-algo auth-key enc-algo enc-key
		s := strings.Split(scanner.Text(), " ")
		if len(s) < 4 {
			return 0, fmt.Errorf("missing IPSec keys or invalid format")
		}

		spiI, err := strconv.Atoi(s[0])
		if err != nil {
			// If no version info is provided assume using key format without
			// versioning and assign SPI.
			spiI = 1
			offset = -1
		}
		if spiI > linux_defaults.IPsecMaxKeyVersion {
			return 0, fmt.Errorf("encryption Key space exhausted, id must be nonzero and less than %d. Attempted %q", linux_defaults.IPsecMaxKeyVersion, s[0])
		}
		if spiI == 0 {
			return 0, fmt.Errorf("zero is not a valid key to disable encryption use `--enable-ipsec=false`, id must be nonzero and less than %d. Attempted %q", linux_defaults.IPsecMaxKeyVersion, s[0])
		}
		spi = uint8(spiI)

		authkey, err := decodeIPSecKey(s[2+offset])
		if err != nil {
			return 0, fmt.Errorf("unable to decode authkey string %q", s[1+offset])
		}
		authname := s[1+offset]

		enckey, err := decodeIPSecKey(s[4+offset])
		if err != nil {
			return 0, fmt.Errorf("unable to decode enckey string %q", s[3+offset])
		}
		encname := s[3+offset]

		ipSecKey.Auth = &netlink.XfrmStateAlgo{
			Name: authname,
			Key:  authkey,
		}
		ipSecKey.Crypt = &netlink.XfrmStateAlgo{
			Name: encname,
			Key:  enckey,
		}
		ipSecKey.Spi = spi

		if len(s) == 6+offset {
			if ipSecKeysGlobal[s[5+offset]] != nil {
				oldSpi = ipSecKeysGlobal[s[5+offset]].Spi
			}
			ipSecKeysGlobal[s[5+offset]] = ipSecKey
		} else {
			if ipSecKeysGlobal[""] != nil {
				oldSpi = ipSecKeysGlobal[""].Spi
			}
			ipSecKeysGlobal[""] = ipSecKey
		}

		scopedLog.WithError(err).Warning("newtimer: oldSPI %u new spi %u", oldSpi, ipSecKey.Spi)
		// Detect a version change and call cleanup routine to remove old
		// keys after a timeout period. We also want to ensure on restart
		// we remove any stale keys for example when a restart changes keys.
		// In the restart case oldSpi will be '0' and cause the delete logic
		// to run.
		if oldSpi != ipSecKey.Spi {
			go func() {
				time.Sleep(linux_defaults.IPsecKeyDeleteDelay)
				scopedLog.Info("New encryption keys reclaiming SPI")
				ipsecDeleteXfrmSpi(ipSecKey.Spi)
			}()
		}
	}
	return spi, nil
}

// EnableIPv6Forwarding sets proc file to enable IPv6 forwarding
func EnableIPv6Forwarding() error {
	ip6ConfPath := "/proc/sys/net/ipv6/conf/"
	device := "all"
	forwarding := "forwarding"
	forwardingOn := "1"
	path := filepath.Join(ip6ConfPath, device, forwarding)
	return ioutil.WriteFile(path, []byte(forwardingOn), 0644)
}