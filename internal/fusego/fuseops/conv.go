// Copyright 2023 Vitaliy Filippov
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

package fuseops

import (
	"time"
	"syscall"

	"github.com/jacobsa/fuse/internal/fusekernel"
)

////////////////////////////////////////////////////////////////////////
// General conversions
////////////////////////////////////////////////////////////////////////

func ConvertTime(t time.Time) (secs uint64, nsec uint32) {
	totalNano := t.UnixNano()
	secs = uint64(totalNano / 1e9)
	nsec = uint32(totalNano % 1e9)
	return secs, nsec
}

func ConvertAttributes(
	inodeID InodeID,
	in *InodeAttributes,
	out *fusekernel.Attr) {
	out.Ino = uint64(inodeID)
	out.Size = in.Size
	out.Atime, out.AtimeNsec = ConvertTime(in.Atime)
	out.Mtime, out.MtimeNsec = ConvertTime(in.Mtime)
	out.Ctime, out.CtimeNsec = ConvertTime(in.Ctime)
	out.SetCrtime(ConvertTime(in.Crtime))
	out.Nlink = in.Nlink
	out.Uid = in.Uid
	out.Gid = in.Gid
	// round up to the nearest 512 boundary
	out.Blocks = (in.Size + 512 - 1) / 512

	// Set the mode.
	out.Mode = ConvertGoMode(in.Mode)

	if out.Mode & (syscall.S_IFCHR | syscall.S_IFBLK) != 0 {
		out.Rdev = in.Rdev
	}
}

// Convert an absolute cache expiration time to a relative time from now for
// consumption by the fuse kernel module.
func ConvertExpirationTime(t time.Time) (secs uint64, nsecs uint32) {
	// Fuse represents durations as unsigned 64-bit counts of seconds and 32-bit
	// counts of nanoseconds (https://tinyurl.com/4muvkr6k). So negative
	// durations are right out. There is no need to cap the positive magnitude,
	// because 2^64 seconds is well longer than the 2^63 ns range of
	// time.Duration.
	d := t.Sub(time.Now())
	if d > 0 {
		secs = uint64(d / time.Second)
		nsecs = uint32((d % time.Second) / time.Nanosecond)
	}

	return secs, nsecs
}

func ConvertChildInodeEntry(
	in *ChildInodeEntry,
	out *fusekernel.EntryOut) {
	out.Nodeid = uint64(in.Child)
	out.Generation = uint64(in.Generation)
	out.EntryValid, out.EntryValidNsec = ConvertExpirationTime(in.EntryExpiration)
	out.AttrValid, out.AttrValidNsec = ConvertExpirationTime(in.AttributesExpiration)

	ConvertAttributes(in.Child, &in.Attributes, &out.Attr)
}
