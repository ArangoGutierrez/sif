// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// Copyright (c) 2017, SingularityWare, LLC. All rights reserved.
// Copyright (c) 2017, Yannick Cote <yhcote@gmail.com> All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package sif

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"strconv"
	"time"
)

// Find next offset aligned to block size
func nextAligned(offset int64, align int) int64 {
	align64 := uint64(align)
	offset64 := uint64(offset)

	if offset64%align64 != 0 {
		offset64 = (offset64 & ^(align64 - 1)) + align64
	}

	return int64(offset64)
}

// Set file pointer offset to next aligned block
func setFileOffNA(fimg *FileImage, alignment int) (int64, error) {
	offset, err := fimg.Fp.Seek(0, 1) // get current position
	if err != nil {
		return -1, fmt.Errorf("seek() getting current file position: %s", err)
	}
	aligned := nextAligned(offset, alignment)
	offset, err = fimg.Fp.Seek(aligned, 0) // set new position
	if err != nil {
		return -1, fmt.Errorf("seek() getting current file position: %s", err)
	}
	return offset, nil
}

// Get current user and returns both uid and gid
func getUserIDs() (int64, int64, error) {
	u, err := user.Current()
	if err != nil {
		return -1, -1, fmt.Errorf("getting current user info: %s", err)
	}

	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return -1, -1, fmt.Errorf("converting UID: %s", err)
	}

	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return -1, -1, fmt.Errorf("converting GID: %s", err)
	}

	return int64(uid), int64(gid), nil
}

// Fill all of the fields of a Descriptor
func fillDescriptor(fimg *FileImage, index int, input DescriptorInput) (err error) {
	descr := &fimg.DescrArr[index]

	curoff, err := fimg.Fp.Seek(0, 1)
	if err != nil {
		return fmt.Errorf("while file pointer look at: %s", err)
	}

	descr.Datatype = input.Datatype
	descr.ID = uint32(index) + 1
	descr.Used = true
	descr.Groupid = input.Groupid
	descr.Link = input.Link
	descr.Fileoff, err = setFileOffNA(fimg, os.Getpagesize())
	if err != nil {
		return
	}
	descr.Filelen = input.Size
	descr.Storelen = descr.Fileoff + descr.Filelen - curoff
	descr.Ctime = time.Now().Unix()
	descr.Mtime = time.Now().Unix()
	descr.UID, descr.Gid, err = getUserIDs()
	if err != nil {
		return fmt.Errorf("filling descriptor: %s", err)
	}
	copy(descr.Name[:DescrNameLen], path.Base(input.Fname))
	copy(descr.Extra[:DescrMaxPrivLen], input.Extra.Bytes())

	return
}

// Write new data object to the SIF file
func writeDataObject(fimg *FileImage, input DescriptorInput) error {
	// if we have bytes in input.data use that instead of an input file
	if input.Data != nil {
		if _, err := fimg.Fp.Write(input.Data); err != nil {
			return fmt.Errorf("copying data object data to SIF file: %s", err)
		}
	} else {
		if n, err := io.Copy(fimg.Fp, input.Fp); err != nil {
			return fmt.Errorf("copying data object file to SIF file: %s", err)
		} else if n != input.Size {
			return fmt.Errorf("short write while copying to SIF file")
		}
	}

	return nil
}

// Find a free descriptor and create a memory representation for addition to the SIF file
func createDescriptor(fimg *FileImage, input DescriptorInput) (err error) {
	var (
		idx int
		v   Descriptor
	)

	if fimg.Header.Dfree == 0 {
		return fmt.Errorf("no descriptor table free entry")
	}

	// look for a free entry in the descriptor table
	for idx, v = range fimg.DescrArr {
		if v.Used == false {
			break
		}
	}
	if int64(idx) == fimg.Header.Dtotal-1 && fimg.DescrArr[idx].Used == true {
		return fmt.Errorf("no descriptor table free entry, warning: header.Dfree was > 0")
	}

	// fill in SIF file descriptor
	if err = fillDescriptor(fimg, idx, input); err != nil {
		return
	}

	// write data object associated to the descriptor in SIF file
	if err = writeDataObject(fimg, input); err != nil {
		return fmt.Errorf("writing data object for SIF file: %s", err)
	}

	// update some global header fields from adding this new descriptor
	fimg.Header.Dfree--
	fimg.Header.Datalen += fimg.DescrArr[idx].Storelen

	return
}

// Release and write the data object descriptor to backing storage (SIF container file)
func writeDescriptors(fimg *FileImage) error {
	// first, move to descriptor start offset
	if _, err := fimg.Fp.Seek(DescrStartOffset, 0); err != nil {
		return fmt.Errorf("seeking to descriptor start offset: %s", err)
	}

	for _, v := range fimg.DescrArr {
		if err := binary.Write(fimg.Fp, binary.LittleEndian, v); err != nil {
			return fmt.Errorf("binary writing descrtable to buf: %s", err)
		}
	}
	fimg.Header.Descrlen = int64(binary.Size(fimg.DescrArr))

	return nil
}

// Write the global header to file
func writeHeader(fimg *FileImage) error {
	// first, move to descriptor start offset
	if _, err := fimg.Fp.Seek(0, 0); err != nil {
		return fmt.Errorf("seeking to beginning of the file: %s", err)
	}

	if err := binary.Write(fimg.Fp, binary.LittleEndian, fimg.Header); err != nil {
		return fmt.Errorf("binary writing header to buf: %s", err)
	}

	return nil
}

// CreateContainer is responsible for the creation of a new SIF container
// file. It takes the creation information specification as input
// and produces an output file as specified in the input data.
func CreateContainer(cinfo CreateInfo) (err error) {
	var fimg FileImage
	fimg.DescrArr = make([]Descriptor, DescrNumEntries)

	if cinfo.Inputlist.Len() == 0 {
		return fmt.Errorf("need at least one input descriptor")
	}

	// Prepare a fresh global header
	copy(fimg.Header.Launch[:], cinfo.Launchstr)
	copy(fimg.Header.Magic[:], HdrMagic)
	copy(fimg.Header.Version[:], cinfo.Sifversion)
	copy(fimg.Header.Arch[:], cinfo.Arch)
	copy(fimg.Header.ID[:], cinfo.ID[:])
	fimg.Header.Ctime = time.Now().Unix()
	fimg.Header.Mtime = time.Now().Unix()
	fimg.Header.Dfree = DescrNumEntries
	fimg.Header.Dtotal = DescrNumEntries
	fimg.Header.Descroff = DescrStartOffset
	fimg.Header.Dataoff = DataStartOffset

	// Create container file
	fimg.Fp, err = os.OpenFile(cinfo.Pathname, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return fmt.Errorf("container file creation failed: %s", err)
	}
	defer fimg.Fp.Close()

	// set file pointer to start of data section */
	if _, err = fimg.Fp.Seek(DataStartOffset, 0); err != nil {
		return fmt.Errorf("setting file offset pointer to DataStartOffset: %s", err)
	}

	for e := cinfo.Inputlist.Front(); e != nil; e = e.Next() {
		// extract the descriptor input info from the list element
		input, ok := e.Value.(DescriptorInput)
		if ok == false {
			return fmt.Errorf("structure is not of expected DescriptorInput type")
		}

		if err = createDescriptor(&fimg, input); err != nil {
			return
		}
	}

	// Write down the descriptor array
	if err = writeDescriptors(&fimg); err != nil {
		return
	}

	// Write down global header to file
	if err = writeHeader(&fimg); err != nil {
		return
	}

	return
}

func zeroData(fimg *FileImage, descr *Descriptor) error {
	// first, move to data object offset
	if _, err := fimg.Fp.Seek(descr.Fileoff, 0); err != nil {
		return fmt.Errorf("seeking to data object offset: %s", err)
	}

	var zero [4096]byte
	n := descr.Filelen
	upbound := int64(4096)
	for {
		if n < 4096 {
			upbound = n
		}

		if _, err := fimg.Fp.Write(zero[:upbound]); err != nil {
			return fmt.Errorf("writing 0's to data object")
		}
		n -= 4096
		if n <= 0 {
			break
		}
	}

	return nil
}

func resetDescriptor(fimg *FileImage, index int) error {
	offset := fimg.Header.Descroff + int64(index)*int64(binary.Size(fimg.DescrArr[0]))

	// first, move to descriptor offset
	if _, err := fimg.Fp.Seek(offset, 0); err != nil {
		return fmt.Errorf("seeking to descriptor: %s", err)
	}

	var emptyDesc Descriptor
	if err := binary.Write(fimg.Fp, binary.LittleEndian, emptyDesc); err != nil {
		return fmt.Errorf("binary writing empty descriptor: %s", err)
	}

	return nil
}

// AddObject add a new data object and its descriptor into the specified SIF file.
func (fimg *FileImage) AddObject(input DescriptorInput) error {
	// set file pointer to the end of data section */
	if _, err := fimg.Fp.Seek(fimg.Header.Dataoff+fimg.Header.Datalen, 0); err != nil {
		return fmt.Errorf("setting file offset pointer to DataStartOffset: %s", err)
	}

	// create a new descriptor entry from input data
	if err := createDescriptor(fimg, input); err != nil {
		return err
	}

	// write down the descriptor array
	if err := writeDescriptors(fimg); err != nil {
		return err
	}

	fimg.Header.Mtime = time.Now().Unix()
	// write down global header to file
	if err := writeHeader(fimg); err != nil {
		return err
	}

	if err := fimg.Fp.Sync(); err != nil {
		return fmt.Errorf("while sync'ing new data object to SIF file: %s", err)
	}

	return nil
}

// DeleteObject removes data from a SIF file referred to by id. The descriptor for the
// data object is free'd and can be reused later. There's currenly 2 clean mode specified
// by flags: DelZero, to zero out the data region for security and DelCompact to
// remove and shink the file compacting the unused area.
func (fimg *FileImage) DeleteObject(id uint32, flags int) error {
	descr, index, err := fimg.GetFromDescrID(id)
	if err != nil {
		return err
	}

	switch flags {
	case DelZero:
		if err = zeroData(fimg, descr); err != nil {
			return err
		}
	case DelCompact:
		return fmt.Errorf("method (DelCompact) not implemented yet")
	}

	// update some global header fields from deleting this descriptor
	fimg.Header.Dfree++
	fimg.Header.Mtime = time.Now().Unix()

	// zero out the unused descriptor
	if err = resetDescriptor(fimg, index); err != nil {
		return err
	}

	// update global header
	if err = writeHeader(fimg); err != nil {
		return err
	}

	if err := fimg.Fp.Sync(); err != nil {
		return fmt.Errorf("while sync'ing deleted data object to SIF file: %s", err)
	}

	return nil
}
