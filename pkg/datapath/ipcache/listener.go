// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ipcache

import (
	"log/slog"
	"net"

	"github.com/cilium/cilium/pkg/bpf"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/datapath/tunnel"
	"github.com/cilium/cilium/pkg/ipcache"
	"github.com/cilium/cilium/pkg/logging/logfields"
	ipcacheMap "github.com/cilium/cilium/pkg/maps/ipcache"
	monitorAPI "github.com/cilium/cilium/pkg/monitor/api"
	"github.com/cilium/cilium/pkg/node"
)

// monitorNotify is an interface to notify the monitor about ipcache changes.
type monitorNotify interface {
	SendEvent(typ int, event any) error
}

type Map interface {
	Update(key bpf.MapKey, value bpf.MapValue) error
	Delete(key bpf.MapKey) error
}

// BPFListener implements the ipcache.IPIdentityMappingBPFListener
// interface with an IPCache store that is backed by BPF maps.
type BPFListener struct {
	logger *slog.Logger
	// bpfMap is the BPF map that this listener will update when events are
	// received from the IPCache.
	bpfMap Map

	// monitorNotify is used to notify the monitor about ipcache updates
	monitorNotify monitorNotify

	// tunnelConf holds the tunneling configuration.
	tunnelConf tunnel.Config
}

// NewListener returns a new listener to push IPCache entries into BPF maps.
func NewListener(m Map, mn monitorNotify, tunnelConf tunnel.Config, logger *slog.Logger) *BPFListener {
	return &BPFListener{
		logger:        logger,
		bpfMap:        m,
		monitorNotify: mn,
		tunnelConf:    tunnelConf,
	}
}

func (l *BPFListener) notifyMonitor(modType ipcache.CacheModification,
	cidr net.IPNet, oldHostIP, newHostIP net.IP, oldID *ipcache.Identity,
	newID ipcache.Identity, encryptKey uint8, k8sMeta *ipcache.K8sMetadata) {
	var (
		k8sNamespace, k8sPodName string
		newIdentity, oldIdentity uint32
		oldIdentityPtr           *uint32
	)

	if l.monitorNotify == nil {
		return
	}

	if k8sMeta != nil {
		k8sNamespace = k8sMeta.Namespace
		k8sPodName = k8sMeta.PodName
	}

	newIdentity = newID.ID.Uint32()
	if oldID != nil {
		oldIdentity = oldID.ID.Uint32()
		oldIdentityPtr = &oldIdentity
	}

	switch modType {
	case ipcache.Upsert:
		msg := monitorAPI.IPCacheUpsertedMessage(cidr.String(), newIdentity, oldIdentityPtr,
			newHostIP, oldHostIP, encryptKey, k8sNamespace, k8sPodName)
		l.monitorNotify.SendEvent(monitorAPI.MessageTypeAgent, msg)
	case ipcache.Delete:
		msg := monitorAPI.IPCacheDeletedMessage(cidr.String(), newIdentity, oldIdentityPtr,
			newHostIP, oldHostIP, encryptKey, k8sNamespace, k8sPodName)
		l.monitorNotify.SendEvent(monitorAPI.MessageTypeAgent, msg)
	}
}

// OnIPIdentityCacheChange is called whenever there is a change of state in the
// IPCache (pkg/ipcache).
// TODO (FIXME): GH-3161.
//
// 'oldIPIDPair' is ignored here, because in the BPF maps an update for the
// IP->ID mapping will replace any existing contents; knowledge of the old pair
// is not required to upsert the new pair.
func (l *BPFListener) OnIPIdentityCacheChange(modType ipcache.CacheModification, cidrCluster cmtypes.PrefixCluster,
	oldHostIP, newHostIP net.IP, oldID *ipcache.Identity, newID ipcache.Identity,
	encryptKey uint8, k8sMeta *ipcache.K8sMetadata, endpointFlags uint8) {
	cidr := cidrCluster.AsIPNet()

	scopedLog := l.logger.With(
		logfields.IPAddr, cidr,
		logfields.Identity, newID,
		logfields.Modification, modType,
	)

	scopedLog.Debug("Daemon notified of IP-Identity cache state change")

	l.notifyMonitor(modType, cidr, oldHostIP, newHostIP, oldID, newID, encryptKey, k8sMeta)

	// TODO - see if we can factor this into an interface under something like
	// pkg/datapath instead of in the daemon directly so that the code is more
	// logically located.

	// Update BPF Maps.

	key := ipcacheMap.NewKey(cidr.IP, cidr.Mask, uint16(cidrCluster.ClusterID()))

	switch modType {
	case ipcache.Upsert:
		var tunnelEndpoint net.IP
		if newHostIP != nil {
			// If the hostIP is specified and it doesn't point to
			// the local host, then the ipcache should be populated
			// with the hostIP so that this traffic can be guided
			// to a tunnel endpoint destination.
			switch l.tunnelConf.UnderlayProtocol() {
			case tunnel.IPv4:
				nodeIPv4 := node.GetIPv4()
				if ip4 := newHostIP.To4(); ip4 != nil && !ip4.Equal(nodeIPv4) {
					tunnelEndpoint = ip4
				}
			case tunnel.IPv6:
				nodeIPv6 := node.GetIPv6()
				if !newHostIP.Equal(nodeIPv6) {
					tunnelEndpoint = newHostIP
				}
			}
		}
		value := ipcacheMap.NewValue(uint32(newID.ID), tunnelEndpoint, encryptKey,
			ipcacheMap.RemoteEndpointInfoFlags(endpointFlags))
		err := l.bpfMap.Update(&key, &value)
		if err != nil {
			scopedLog.Warn(
				"unable to update bpf map",
				logfields.Error, err,
				logfields.Key, key,
				logfields.Value, value,
			)
		}
	case ipcache.Delete:
		err := l.bpfMap.Delete(&key)
		if err != nil {
			scopedLog.Warn(
				"unable to delete from bpf map",
				logfields.Key, key,
			)
		}
	default:
		scopedLog.Warn("cache modification type not supported")
	}
}
