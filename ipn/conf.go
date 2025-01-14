// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ipn

import (
	"net/netip"

	"tailscale.com/tailcfg"
	"tailscale.com/types/opt"
	"tailscale.com/types/preftype"
)

// ConfigVAlpha is the config file format for the "alpha0" version.
type ConfigVAlpha struct {
	Locked opt.Bool `json:",omitempty"` // whether the config is locked from being changed by 'tailscale set'; it defaults to true

	ServerURL *string  `json:",omitempty"` // defaults to https://controlplane.tailscale.com
	AuthKey   *string  `json:",omitempty"` // as needed if NeedsLogin. either key or path to a file (if it contains a slash)
	Enabled   opt.Bool `json:",omitempty"` // wantRunning; empty string defaults to true

	OperatorUser *string `json:",omitempty"` // local user name who is allowed to operate tailscaled without being root or using sudo
	Hostname     *string `json:",omitempty"`

	AcceptDNS    opt.Bool `json:"acceptDNS,omitempty"` // --accept-dns
	AcceptRoutes opt.Bool `json:"acceptRoutes,omitempty"`

	ExitNode                   *string  `json:"exitNode,omitempty"` // IP, StableID, or MagicDNS base name
	AllowLANWhileUsingExitNode opt.Bool `json:"allowLANWhileUsingExitNode,omitempty"`

	AdvertiseRoutes []netip.Prefix `json:",omitempty"`
	DisableSNAT     opt.Bool       `json:",omitempty"`

	NetfilterMode *string `json:",omitempty"` // "on", "off", "nodivert"

	PostureChecking opt.Bool         `json:",omitempty"`
	RunSSHServer    opt.Bool         `json:",omitempty"` // Tailscale SSH
	ShieldsUp       opt.Bool         `json:",omitempty"`
	AutoUpdate      *AutoUpdatePrefs `json:",omitempty"`
	ServeConfigTemp *ServeConfig     `json:",omitempty"` // TODO(bradfitz,maisem): make separate stable type for this

	// TODO(bradfitz,maisem): future something like:
	// Profile map[string]*Config // keyed by alice@gmail.com, corp.com (TailnetSID)
}

func (c *ConfigVAlpha) ToPrefs() (MaskedPrefs, error) {
	var mp MaskedPrefs
	if c == nil {
		return mp, nil
	}
	if c.ServerURL != nil {
		mp.ControlURL = *c.ServerURL
		mp.ControlURLSet = true
	}
	if c.Enabled != "" {
		mp.WantRunning = c.Enabled.EqualBool(true)
		mp.WantRunningSet = true
	}
	if c.OperatorUser != nil {
		mp.OperatorUser = *c.OperatorUser
		mp.OperatorUserSet = true
	}
	if c.Hostname != nil {
		mp.Hostname = *c.Hostname
		mp.HostnameSet = true
	}
	if c.AcceptDNS != "" {
		mp.CorpDNS = c.AcceptDNS.EqualBool(true)
		mp.CorpDNSSet = true
	}
	if c.AcceptRoutes != "" {
		mp.RouteAll = c.AcceptRoutes.EqualBool(true)
		mp.RouteAllSet = true
	}
	if c.ExitNode != nil {
		ip, err := netip.ParseAddr(*c.ExitNode)
		if err == nil {
			mp.ExitNodeIP = ip
			mp.ExitNodeIPSet = true
		} else {
			mp.ExitNodeID = tailcfg.StableNodeID(*c.ExitNode)
			mp.ExitNodeIDSet = true
		}
	}
	if c.AllowLANWhileUsingExitNode != "" {
		mp.ExitNodeAllowLANAccess = c.AllowLANWhileUsingExitNode.EqualBool(true)
		mp.ExitNodeAllowLANAccessSet = true
	}
	if c.AdvertiseRoutes != nil {
		mp.AdvertiseRoutes = c.AdvertiseRoutes
		mp.AdvertiseRoutesSet = true
	}
	if c.DisableSNAT != "" {
		mp.NoSNAT = c.DisableSNAT.EqualBool(true)
		mp.NoSNAT = true
	}
	if c.NetfilterMode != nil {
		m, err := preftype.ParseNetfilterMode(*c.NetfilterMode)
		if err != nil {
			return mp, err
		}
		mp.NetfilterMode = m
		mp.NetfilterModeSet = true
	}
	if c.PostureChecking != "" {
		mp.PostureChecking = c.PostureChecking.EqualBool(true)
		mp.PostureCheckingSet = true
	}
	if c.RunSSHServer != "" {
		mp.RunSSH = c.RunSSHServer.EqualBool(true)
		mp.RunSSHSet = true
	}
	if c.ShieldsUp != "" {
		mp.ShieldsUp = c.ShieldsUp.EqualBool(true)
		mp.ShieldsUpSet = true
	}
	if c.AutoUpdate != nil {
		mp.AutoUpdate = *c.AutoUpdate
		mp.AutoUpdateSet = true
	}
	return mp, nil
}
