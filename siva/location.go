package siva

import (
	"os"

	borges "github.com/src-d/go-borges"
	sivafs "gopkg.in/src-d/go-billy-siva.v4"
	billy "gopkg.in/src-d/go-billy.v4"
	"gopkg.in/src-d/go-billy.v4/memfs"
	"gopkg.in/src-d/go-errors.v1"
	"gopkg.in/src-d/go-git.v4/config"
)

// ErrMalformedData when checkpoint data is invalid.
var ErrMalformedData = errors.NewKind("malformed data")

// Location represents a siva file archiving several git repositories.
type Location struct {
	id         borges.LocationID
	path       string
	cachedFS   sivafs.SivaFS
	lib        *Library
	checkpoint *checkpoint
	txer       *transactioner
}

var _ borges.Location = (*Location)(nil)

// NewLocation creates a new Location struct. If create is true and the siva
// file does not exist a new siva file is created.
func NewLocation(
	id borges.LocationID,
	lib *Library,
	path string,
	create bool,
) (*Location, error) {
	cp, err := newCheckpoint(lib.fs, path, create)
	if err != nil {
		return nil, err
	}

	loc := &Location{
		id:         id,
		path:       path,
		lib:        lib,
		checkpoint: cp,
	}

	loc.txer = newTransactioner(loc, lib.locReg)
	return loc, nil
}

func (l *Location) newFS() (sivafs.SivaFS, error) {
	return sivafs.NewFilesystem(l.baseFS(), l.path, memfs.New())
}

// FS returns a filesystem for the location's siva file.
func (l *Location) FS() (sivafs.SivaFS, error) {
	if l.cachedFS != nil {
		return l.cachedFS, nil
	}

	if err := l.checkpoint.Apply(); err != nil {
		return nil, err
	}

	sfs, err := l.newFS()
	if err != nil {
		return nil, err
	}

	l.cachedFS = sfs
	return sfs, nil
}

// ID implements the borges.Location interface.
func (l *Location) ID() borges.LocationID {
	return l.id
}

// Init implements the borges.Location interface.
func (l *Location) Init(id borges.RepositoryID) (borges.Repository, error) {
	has, err := l.Has(id)
	if err != nil {
		return nil, err
	}
	if has {
		return nil, borges.ErrRepositoryExists.New(id)
	}

	repo, err := l.repository(id, borges.RWMode)
	if err != nil {
		return nil, err
	}

	cfg := &config.RemoteConfig{
		Name: id.String(),
		URLs: []string{id.String()},
	}

	_, err = repo.R().CreateRemote(cfg)
	if err != nil {
		return nil, err
	}

	return repo, nil
}

// Get implements the borges.Location interface.
func (l *Location) Get(
	id borges.RepositoryID,
	mode borges.Mode,
) (borges.Repository, error) {
	has, err := l.Has(id)
	if err != nil {
		return nil, err
	}

	if !has {
		return nil, borges.ErrRepositoryNotExists.New(id)
	}

	return l.repository(id, mode)
}

// GetOrInit implements the borges.Location interface.
func (l *Location) GetOrInit(id borges.RepositoryID) (borges.Repository, error) {
	has, err := l.Has(id)
	if err != nil {
		return nil, err
	}

	if has {
		return l.repository(id, borges.RWMode)
	}

	return l.Init(id)
}

// Has implements the borges.Location interface.
func (l *Location) Has(name borges.RepositoryID) (bool, error) {
	if l.cachedFS == nil {
		// Return false when the siva file does not exist. If repository is
		// called it will create a new siva file.
		_, err := l.baseFS().Stat(l.path)
		if err != nil {
			if os.IsNotExist(err) {
				return false, nil
			}
			return false, err
		}
	}

	repo, err := l.repository("", borges.ReadOnlyMode)
	if err != nil {
		return false, err
	}
	config, err := repo.R().Config()
	if err != nil {
		return false, err
	}

	for _, r := range config.Remotes {
		if len(r.URLs) > 0 {
			id := toRepoID(r.URLs[0])
			if id == name {
				return true, nil
			}
		}
	}

	return false, nil
}

// Repositories implements the borges.Location interface.
func (l *Location) Repositories(mode borges.Mode) (borges.RepositoryIterator, error) {
	var remotes []*config.RemoteConfig

	if l.cachedFS == nil {
		// Return false when the siva file does not exist. If repository is
		// called it will create a new siva file.
		_, err := l.baseFS().Stat(l.path)
		if err != nil {
			if os.IsNotExist(err) {
				return &repositoryIterator{
					mode:    mode,
					l:       l,
					pos:     0,
					remotes: remotes,
				}, nil
			}
			return nil, err
		}
	}

	repo, err := l.repository("", borges.ReadOnlyMode)
	if err != nil {
		return nil, err
	}
	cfg, err := repo.R().Config()
	if err != nil {
		return nil, err
	}

	for _, r := range cfg.Remotes {
		remotes = append(remotes, r)
	}

	return &repositoryIterator{
		mode:    mode,
		l:       l,
		pos:     0,
		remotes: remotes,
	}, nil
}

func (l *Location) baseFS() billy.Filesystem {
	return l.lib.fs
}

func (l *Location) getRepoFs(id borges.RepositoryID, mode borges.Mode) (sivafs.SivaFS, error) {
	if !l.lib.transactional || mode != borges.RWMode {
		return l.FS()
	}

	if err := l.txer.Start(); err != nil {
		return nil, err
	}

	fs, err := l.newFS()
	if err != nil {
		return nil, err
	}

	if err := l.checkpoint.Save(); err != nil {
		return nil, err
	}

	return fs, nil
}

// Commit persists transactional or write operations performed on the repositories.
func (l *Location) Commit(mode borges.Mode) error {
	if !l.lib.transactional || mode != borges.RWMode {
		return nil
	}

	defer l.txer.Stop()
	if err := l.checkpoint.Reset(); err != nil {
		return err
	}

	l.cachedFS = nil
	return nil
}

// Rollback discard transactional or write operations performed on the repositories.
func (l *Location) Rollback(mode borges.Mode) error {
	if !l.lib.transactional || mode != borges.RWMode {
		return nil
	}

	defer l.txer.Stop()
	if err := l.checkpoint.Apply(); err != nil {
		return err
	}

	l.cachedFS = nil
	return nil
}

func (l *Location) repository(
	id borges.RepositoryID,
	mode borges.Mode,
) (borges.Repository, error) {
	fs, err := l.getRepoFs(id, mode)
	if err != nil {
		return nil, err
	}

	return NewRepository(id, fs, mode, l)
}
