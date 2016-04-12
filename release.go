package main

import (
	"crypto/rsa"
	"fmt"
	"log"
	"regexp"
	"sort"
	"sync"

	"github.com/blang/semver"
	"github.com/google/go-github/github"
	"github.com/yinghuocho/autoupdate-server/args"
)

var (
	updateAssetRe = regexp.MustCompile(`^update_(darwin|windows|linux)_(arm|386|amd64)\.?.*$`)
	emptyVersion  semver.Version
)

// Arch holds architecture names.
var Arch = struct {
	X64 string
	X86 string
	ARM string
}{
	"amd64",
	"386",
	"arm",
}

// OS holds operating system names.
var OS = struct {
	Windows string
	Linux   string
	Darwin  string
}{
	"windows",
	"linux",
	"darwin",
}

// Release struct represents a single github release.
type Release struct {
	id      int
	URL     string
	Version semver.Version
	Assets  []Asset
}

type releasesByID []Release

// Asset struct represents a file included as part of a Release.
type Asset struct {
	id        int
	v         semver.Version
	Name      string
	URL       string
	LocalFile string
	Checksum  string
	Signature string
	AssetInfo
}

// AssetInfo struct holds OS and Arch information of an asset.
type AssetInfo struct {
	OS   string
	Arch string
}

// ReleaseManager struct defines a repository to pull releases from.
type ReleaseManager struct {
	client          *github.Client
	owner           string
	repo            string
	assetDir        string
	patchDir        string
	privKey         *rsa.PrivateKey
	updateAssetsMap map[string]map[string]map[string]*Asset
	latestAssetsMap map[string]map[string]*Asset
	mu              *sync.RWMutex
}

func (a releasesByID) Len() int {
	return len(a)
}

func (a releasesByID) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}

func (a releasesByID) Less(i, j int) bool {
	return a[i].id < a[j].id
}

// NewReleaseManager creates a wrapper of github.Client.
func NewReleaseManager(owner string, repo string, assetDir string, patchDir string, privKey *rsa.PrivateKey) *ReleaseManager {

	ghc := &ReleaseManager{
		client:          github.NewClient(nil),
		owner:           owner,
		repo:            repo,
		assetDir:        assetDir,
		patchDir:        patchDir,
		privKey:         privKey,
		mu:              new(sync.RWMutex),
		updateAssetsMap: make(map[string]map[string]map[string]*Asset),
		latestAssetsMap: make(map[string]map[string]*Asset),
	}

	return ghc
}

// getReleases queries github for all product releases.
func (g *ReleaseManager) getReleases() ([]Release, error) {
	var releases []Release

	for page := 1; true; page++ {
		opt := &github.ListOptions{Page: page}

		rels, _, err := g.client.Repositories.ListReleases(g.owner, g.repo, opt)

		if err != nil {
			return nil, err
		}

		if len(rels) == 0 {
			break
		}

		releases = make([]Release, 0, len(rels))

		for i := range rels {
			version := *rels[i].TagName
			v, err := semver.Parse(version)
			if err != nil {
				log.Printf("Release %q is not semantically versioned (%q). Skipping.", version, err)
				continue
			}
			rel := Release{
				id:      *rels[i].ID,
				URL:     *rels[i].ZipballURL,
				Version: v,
			}
			rel.Assets = make([]Asset, 0, len(rels[i].Assets))
			for _, asset := range rels[i].Assets {
				rel.Assets = append(rel.Assets, Asset{
					id:   *asset.ID,
					Name: *asset.Name,
					URL:  *asset.BrowserDownloadURL,
				})
			}
			log.Printf("Release %q has %d assets...", version, len(rel.Assets))
			releases = append(releases, rel)
		}
	}

	sort.Sort(sort.Reverse(releasesByID(releases)))

	return releases, nil
}

// UpdateAssetsMap will pull published releases, scan for compatible
// update-only binaries and will add them to the updateAssetsMap.
func (g *ReleaseManager) UpdateAssetsMap() (err error) {

	var rs []Release

	log.Printf("Getting releases...")
	if rs, err = g.getReleases(); err != nil {
		return err
	}

	log.Printf("Getting assets...")
	for i := range rs {
		log.Printf("Getting assets for release %q...", rs[i].Version)
		for j := range rs[i].Assets {
			log.Printf("Found %q.", rs[i].Assets[j].Name)
			// Does this asset represent a binary update?
			if isUpdateAsset(rs[i].Assets[j].Name) {
				log.Printf("%q is an auto-update asset.", rs[i].Assets[j].Name)
				asset := rs[i].Assets[j]
				asset.v = rs[i].Version
				info, err := getAssetInfo(asset.Name)
				if err != nil {
					return fmt.Errorf("Could not get asset info: %q", err)
				}
				if err = g.pushAsset(info.OS, info.Arch, &asset); err != nil {
					return fmt.Errorf("Could not push asset: %q", err)
				}
			} else {
				log.Printf("%q is not an auto-update asset. Skipping.", rs[i].Assets[j].Name)
			}
		}
	}

	return nil
}

func (g *ReleaseManager) getProductUpdate(os string, arch string) (asset *Asset, err error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.latestAssetsMap == nil {
		return nil, fmt.Errorf("No updates available.")
	}

	if g.latestAssetsMap[os] == nil {
		return nil, fmt.Errorf("No such OS.")
	}

	if g.latestAssetsMap[os][arch] == nil {
		return nil, fmt.Errorf("No such Arch.")
	}

	return g.latestAssetsMap[os][arch], nil
}

func (g *ReleaseManager) lookupAssetWithChecksum(os string, arch string, checksum string) (asset *Asset, err error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.updateAssetsMap == nil {
		return nil, fmt.Errorf("No updates available.")
	}

	if g.updateAssetsMap[os] == nil {
		return nil, fmt.Errorf("No such OS.")
	}

	if g.updateAssetsMap[os][arch] == nil {
		return nil, fmt.Errorf("No such Arch.")
	}

	for _, a := range g.updateAssetsMap[os][arch] {
		if a.Checksum == checksum {
			return a, nil
		}
	}

	return nil, fmt.Errorf("Could not find a matching checksum in assets list.")
}

func (g *ReleaseManager) pushAsset(os string, arch string, asset *Asset) (err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	version := asset.v

	asset.OS = os
	asset.Arch = arch

	if version.EQ(emptyVersion) {
		return fmt.Errorf("Missing asset version.")
	}

	var localfile string
	if localfile, err = downloadAsset(asset.URL, g.assetDir); err != nil {
		return err
	}

	if asset.Checksum, _, err = checksumForFile(localfile); err != nil {
		return err
	}

	if asset.Signature, err = signatureForFile(localfile, g.privKey); err != nil {
		return err
	}

	// Pushing version.
	if g.updateAssetsMap[os] == nil {
		g.updateAssetsMap[os] = make(map[string]map[string]*Asset)
	}
	if g.updateAssetsMap[os][arch] == nil {
		g.updateAssetsMap[os][arch] = make(map[string]*Asset)
	}
	g.updateAssetsMap[os][arch][version.String()] = asset

	// Setting latest version.
	if g.latestAssetsMap[os] == nil {
		g.latestAssetsMap[os] = make(map[string]*Asset)
	}
	if g.latestAssetsMap[os][arch] == nil {
		g.latestAssetsMap[os][arch] = asset
	} else {
		// Compare against already set version
		if asset.v.GT(g.latestAssetsMap[os][arch].v) {
			g.latestAssetsMap[os][arch] = asset
		}
	}

	return nil
}

// CheckForUpdate receives a *Params message and emits a *Result. If both res
// and err are nil it means no update is available.
func (g *ReleaseManager) CheckForUpdate(p *args.Params) (res *args.Result, err error) {
	log.Printf("%v", p)
	// Keep for the future.
	if p.Version < 1 {
		p.Version = 1
	}

	// p must not be nil.
	if p == nil {
		return nil, fmt.Errorf("Expecting params")
	}

	if p.Tags != nil {
		// Compatibility with go-check.
		if p.Tags["os"] != "" {
			p.OS = p.Tags["os"]
		}
		if p.Tags["arch"] != "" {
			p.Arch = p.Tags["arch"]
		}
	}

	appVersion, err := semver.Parse(p.AppVersion)
	if err != nil {
		return nil, fmt.Errorf("Bad version string: %v", err)
	}

	if p.Checksum == "" {
		return nil, fmt.Errorf("Checksum must not be nil")
	}

	if p.OS == "" {
		return nil, fmt.Errorf("OS is required")
	}

	if p.Arch == "" {
		return nil, fmt.Errorf("Arch is required")
	}

	// Looking if there is a newer version for the os/arch.
	var update *Asset
	if update, err = g.getProductUpdate(p.OS, p.Arch); err != nil {
		return nil, fmt.Errorf("Could not lookup for updates: %s", err)
	}

	// No update available.
	if update.v.LTE(appVersion) {
		return nil, ErrNoUpdateAvailable
	}

	// Looking for the asset thay matches the current app checksum.
	var current *Asset
	if current, err = g.lookupAssetWithChecksum(p.OS, p.Arch, p.Checksum); err != nil {
		// No such asset with the given checksum, nothing to compare.
		r := &args.Result{
			Initiative: args.INITIATIVE_AUTO,
			URL:        update.URL,
			PatchType:  args.PATCHTYPE_NONE,
			Version:    update.v.String(),
			Checksum:   update.Checksum,
			Signature:  update.Signature,
		}

		return r, nil
	}

	// Generate a binary diff of the two assets.
	var patch *Patch
	log.Printf("Generating patch")
	if patch, err = generatePatch(current.URL, update.URL, g.assetDir, g.patchDir); err != nil {
		return nil, fmt.Errorf("Unable to generate patch: %q", err)
	}

	// Generate result.
	r := &args.Result{
		Initiative: args.INITIATIVE_AUTO,
		URL:        update.URL,
		PatchURL:   patch.File,
		PatchType:  args.PATCHTYPE_BSDIFF,
		Version:    update.v.String(),
		Checksum:   update.Checksum,
		Signature:  update.Signature,
	}

	return r, nil
}

func getAssetInfo(s string) (*AssetInfo, error) {
	matches := updateAssetRe.FindStringSubmatch(s)
	if len(matches) >= 3 {
		if matches[1] != OS.Windows && matches[1] != OS.Linux && matches[1] != OS.Darwin {
			return nil, fmt.Errorf("Unknown OS: \"%s\".", matches[1])
		}
		if matches[2] != Arch.X64 && matches[2] != Arch.X86 && matches[2] != Arch.ARM {
			return nil, fmt.Errorf("Unknown architecture \"%s\".", matches[2])
		}
		info := &AssetInfo{
			OS:   matches[1],
			Arch: matches[2],
		}
		return info, nil
	}
	return nil, fmt.Errorf("Could not find asset info.")
}

func isUpdateAsset(s string) bool {
	return updateAssetRe.MatchString(s)
}
