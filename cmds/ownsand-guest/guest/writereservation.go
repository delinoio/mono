// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"strings"
)

const reservationPrefix = ".ownsand-reserve-"

type writeReservation struct {
	Dir      string       `json:"dir"`
	Identity fileIdentity `json:"identity"`
}

func (r *writeReservation) validFor(staging string) bool {
	parent, name := splitTarget(r.Dir)
	stageParent, stageName := splitTarget(staging)
	return parent == stageParent && name != stageName &&
		strings.HasPrefix(name, reservationPrefix) && checkGuestPath(staging) == nil
}

func (r *writeReservation) file() string {
	return siblingPath(r.Dir, "file")
}

// stageLink exposes the reserved object at the staging path. Tests interrupt
// or fail this operation to exercise the initialization boundary.
var stageLink = os.Link

func (m *writeManager) createStaging(e *writeEntry, parent string) (err error) {
	if e.recordPath == "" {
		e.file, err = os.OpenFile(e.tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		return err
	}

	dir := siblingPath(parent, reservationPrefix+rand.Text())
	if err := mkdirWriteReservation(dir); err != nil {
		return err
	}
	identity, err := identityOfPath(dir)
	if err != nil {
		return err
	}
	e.reservation = &writeReservation{Dir: dir, Identity: identity}

	saved, linked := false, false
	var seed *os.File
	defer func() {
		if seed != nil {
			seed.Close()
		}
		if err == nil {
			return
		}
		e.closeFile()
		e.recordOnly = !linked
		var cleanupErr error
		if saved {
			cleanupErr = m.cleanup(e)
		} else {
			cleanupErr = e.removeReservation()
		}
		if cleanupErr != nil {
			m.logger.Warn("clean up write initialization failed", "write_id", e.id, "error", sanitizedError(cleanupErr))
			if saved {
				e.gone = true
				m.entries[e.id] = e
			}
		}
	}()

	// Record custody of the private directory before creating any file in it.
	if err := saveWriteRecord(e, true); err != nil {
		m.logger.Warn("save write intent failed", "write_id", e.id, "error", sanitizedError(err))
		return err
	}
	saved = true
	seed, err = createWriteSeed(e.reservation.file(), parent)
	if err != nil {
		return err
	}
	identity, err = identityOfFile(seed)
	if err != nil {
		return err
	}
	e.identity = &identity
	// Commit the file identity before creating the staging name. Linking
	// fails if a file already exists there and leaves that file untouched.
	if err := saveWriteRecord(e, true); err != nil {
		m.logger.Warn("save write intent failed", "write_id", e.id, "error", sanitizedError(err))
		return err
	}
	if err := stageLink(e.reservation.file(), e.tmp); err != nil {
		return err
	}

	linked = true
	err = seed.Close()
	seed = nil
	if err != nil {
		return err
	}
	e.file, err = os.OpenFile(e.tmp, stagingReopenFlags, 0)
	if err != nil {
		return err
	}
	current, err := identityOfFile(e.file)
	if err != nil {
		return err
	}
	if current != *e.identity {
		return fs.ErrNotExist
	}
	return nil
}

func checkWriteIdentity(path string, identity fileIdentity) (bool, error) {
	current, err := identityOfPath(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil && current == identity, err
}

// ownsStaging requires the original reservation file to exist. That link
// prevents the filesystem from reusing the original file identity after a
// replacement at the staging path.
func (e *writeEntry) ownsStaging() (bool, error) {
	if e.identity == nil {
		return false, nil
	}
	owned, err := checkWriteIdentity(e.reservation.Dir, e.reservation.Identity)
	if err != nil || !owned {
		return false, err
	}
	return checkWriteIdentity(e.reservation.file(), *e.identity)
}

func (e *writeEntry) removeReservation() error {
	r := e.reservation
	if r == nil {
		return nil
	}
	owned, err := checkWriteIdentity(r.Dir, r.Identity)
	if err != nil || !owned {
		return err
	}

	seed := r.file()
	if e.identity != nil {
		owned, err = checkWriteIdentity(seed, *e.identity)
	} else {
		// The initial intent only authorizes cleanup inside the verified,
		// private directory. The helper creates only one empty regular
		// file there before it records that file's identity.
		var info fs.FileInfo
		info, err = os.Lstat(seed)
		owned = err == nil && info.Mode().IsRegular() && info.Size() == 0
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if owned {
		if err := os.Remove(seed); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// Preserve a reservation that contains any entries we did not verify.
	items, err := os.ReadDir(r.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(items) == 0 {
		if err := os.Remove(r.Dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
