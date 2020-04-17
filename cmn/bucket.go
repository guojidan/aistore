// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"strings"
)

// Cloud Provider enum
const (
	AnyCloud = "cloud" // refers to any 3rd party Cloud

	ProviderAmazon = "aws"
	ProviderGoogle = "gcp"
	ProviderAIS    = "ais"
	ProviderAzure  = "azure"
	allProviders   = "aws, gcp, ais, azure"

	NsUUIDPrefix = '@' // BEWARE: used by on-disk layout
	NsNamePrefix = '#' // BEWARE: used by on-disk layout

	BckProviderSeparator = "://"
)

type (
	// Ns (or Namespace) adds additional layer for scoping the data under
	// the same provider. It allows to have same dataset and bucket names
	// under different namespaces what allows for easy data manipulation without
	// affecting data in different namespaces.
	Ns struct {
		// UUID of other remote AIS cluster (for now only used for AIS). Note
		// that we can have different namespaces which refer to same UUID (cluster).
		// This means that in a sense UUID is a parent of the actual namespace.
		UUID string `json:"uuid"`
		// Name uniquely identifies a namespace under the same UUID (which may
		// be empty) and is used in building FQN for the objects.
		Name string `json:"name"`
	}

	Bck struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Ns       Ns     `json:"namespace"`
	}

	BucketNames []Bck
)

var (
	// NsGlobal represents *this* cluster's global namespace that is used by default when
	// no specific namespace was defined or provided by the user.
	NsGlobal = Ns{}
	// NsGlobalRemote represents combined remote namespaces. As such, NsGlobalRemote applies
	// exclusively to AIS (provider) given that other Cloud providers are remote by definition.
	NsGlobalRemote = Ns{UUID: string(NsUUIDPrefix)}

	Providers = map[string]struct{}{
		ProviderAIS:    {},
		ProviderGoogle: {},
		ProviderAmazon: {},
		ProviderAzure:  {},
	}
)

// Parses [@uuid][#namespace]. It does a little bit more than just parsing
// a string from `Uname` so that logic can be reused in different places.
func ParseNsUname(s string) (n Ns) {
	if len(s) > 0 && s[0] == NsUUIDPrefix {
		s = s[1:]
	}
	idx := strings.IndexByte(s, NsNamePrefix)
	if idx == -1 {
		n.UUID = s
	} else {
		n.UUID = s[:idx]
		n.Name = s[idx+1:]
	}
	return
}

////////
// Ns //
////////

func (n Ns) String() string {
	if n.IsGlobal() {
		return ""
	}
	res := ""
	if n.UUID != "" {
		res += string(NsUUIDPrefix) + n.UUID
	}
	if n.Name != "" {
		res += string(NsNamePrefix) + n.Name
	}
	return res
}

func (n Ns) Uname() string {
	b := make([]byte, 0, 2+len(n.UUID)+len(n.Name))
	b = append(b, NsUUIDPrefix)
	b = append(b, n.UUID...)
	b = append(b, NsNamePrefix)
	b = append(b, n.Name...)
	return string(b)
}

func (n Ns) Validate() error {
	if !nsReg.MatchString(n.UUID) || !nsReg.MatchString(n.Name) {
		return fmt.Errorf(
			"namespace (uuid: %q, name: %q) may only contain letters, numbers, dashes (-), underscores (_)",
			n.UUID, n.Name,
		)
	}
	return nil
}

func (n Ns) Contains(other Ns) bool {
	if n.IsGlobal() {
		return !other.IsRemote()
	}
	if n.IsGlobalRemote() {
		return other.IsRemote()
	}
	return n == other
}

/////////
// Bck //
/////////

// TODO: s/query Bck/query QueryBck/
func (query Bck) Contains(other Bck) bool {
	if query.Name != "" {
		// NOTE: named bucket with no provider is assumed to be ais://
		if other.Provider == "" {
			other.Provider = ProviderAIS
		}
		return query.Equal(other)
	}
	ok := query.Provider == other.Provider ||
		query.Provider == "" ||
		query.Provider == AnyCloud && other.IsCloud()
	return ok && query.Ns.Contains(other.Ns)
}

func (b Bck) Less(other Bck) bool {
	if b.Contains(other) {
		return true
	}
	if b.Provider != other.Provider {
		return b.Provider < other.Provider
	}
	sb, so := b.Ns.String(), other.Ns.String()
	if sb != so {
		return sb < so
	}
	return b.Name < other.Name
}

func (b Bck) Equal(other Bck) bool {
	return b.Name == other.Name && b.Provider == other.Provider && b.Ns == other.Ns
}

func (b Bck) Valid() bool {
	return ValidateBckName(b.Name) == nil && IsValidProvider(b.Provider) && b.Ns.Validate() == nil
}

func (b Bck) String() string {
	if b.Ns.IsGlobal() {
		if b.Provider == "" {
			return b.Name
		}
		return fmt.Sprintf("%s%s%s", b.Provider, BckProviderSeparator, b.Name)
	}
	if b.Provider == "" {
		return fmt.Sprintf("%s/%s", b.Ns, b.Name)
	}
	return fmt.Sprintf("%s%s%s/%s", b.Provider, BckProviderSeparator, b.Ns, b.Name)
}

func (b Bck) IsEmpty() bool { return b.Name == "" && b.Provider == "" && b.Ns == NsGlobal }

//
// Is-Whats
//

func (n Ns) IsGlobal() bool       { return n == NsGlobal }
func (n Ns) IsGlobalRemote() bool { return n == NsGlobalRemote }
func (n Ns) IsRemote() bool       { return n.UUID != "" }

func (b Bck) IsAIS() bool       { return b.Provider == ProviderAIS && !b.Ns.IsRemote() } // is local AIS cluster
func (b Bck) IsRemoteAIS() bool { return b.Provider == ProviderAIS && b.Ns.IsRemote() }  // is remote AIS cluster
func (b Bck) IsRemote() bool    { return b.IsCloud() || b.IsRemoteAIS() }                // is remote
// is 3rd party Cloud
func (b Bck) IsCloud(anyCloud ...string) bool {
	if b.Provider == ProviderAIS {
		return false
	}
	if IsValidProvider(b.Provider) {
		return true
	}
	return len(anyCloud) > 0 && b.Provider == AnyCloud && b.Provider == anyCloud[0]
}
func (b Bck) HasProvider() bool { return b.IsAIS() || b.IsCloud() }

func IsValidProvider(provider string) bool {
	_, ok := Providers[provider]
	return ok
}

/////////////////
// BucketNames //
/////////////////

func (names BucketNames) Len() int {
	return len(names)
}

func (names BucketNames) Less(i, j int) bool {
	return names[i].Less(names[j])
}

func (names BucketNames) Swap(i, j int) {
	names[i], names[j] = names[j], names[i]
}

func (names BucketNames) Select(selbck Bck) (filtered BucketNames) {
	for i, bck := range names {
		if selbck.Contains(bck) {
			if filtered != nil {
				filtered = append(filtered, bck)
			}
		} else if filtered == nil {
			filtered = make(BucketNames, 0, i+1)
			filtered = append(filtered, names[:i]...)
		}
	}
	if filtered == nil {
		return names
	}
	return filtered
}

func (names BucketNames) Contains(other Bck) bool {
	for _, bck := range names {
		if bck.Equal(other) || bck.Contains(other) {
			return true
		}
	}
	return false
}

func (names BucketNames) Equal(other BucketNames) bool {
	if len(names) != len(other) {
		return false
	}
	for _, b1 := range names {
		var found bool
		for _, b2 := range other {
			if b1.Equal(b2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
