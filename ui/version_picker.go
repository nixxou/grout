package ui

import (
	"errors"
	"fmt"
	"grout/cache"
	"grout/internal"
	"grout/internal/fileutil"
	"grout/romm"
	"os"
	"path/filepath"
	"strings"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	buttons "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/constants"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/i18n"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
)

// VersionPickerInput opens the LiteBox version/rom picker for one game. Versions may be handed
// over pre-fetched (Game Options already lists them to decide whether to show its entry); when nil
// they are fetched here.
type VersionPickerInput struct {
	Config   *internal.Config
	Host     romm.Host
	Platform romm.Platform
	Game     romm.Rom
	Versions []romm.LiteBoxVersion
}

// VersionPickerOutput: after a switch, Game is the row this client is now served for the game,
// usually a DIFFERENT rom_id than the one the picker opened on, since the pin changes what the
// server serves (LiteBoxPinResponse.RomID); the old row is gone from the cache.
type VersionPickerOutput struct {
	Action   VersionPickerAction
	Platform romm.Platform
	Game     romm.Rom
}

// VersionPickerScreen is the two-level picker backported from Argosy, fed by the server's own
// eligibility (RommLiteBoxApi.cs, the rule its rom_id generation uses):
//  1. the versions: the game's own file and its Additional Applications. A on a version served
//     whole pins it; A on an ELIGIBLE one (an archive the extractor takes apart for this
//     platform/emulator) opens level 2 instead, because only its roms carry a rom_id;
//  2. the roms inside that one archive. A pins the rom.
//
// A game with a single version that is eligible opens straight on level 2, and B then leaves.
// Otherwise B goes back one level at a time and leaves from level 1, never pinning anything.
// X does what A does, then deletes the local files of the version that was served before.
type VersionPickerScreen struct{}

func NewVersionPickerScreen() *VersionPickerScreen {
	return &VersionPickerScreen{}
}

// versionPickerRow is one line of either level. Drillable rows (eligible versions) open level 2;
// the others name a pinnable choice by appID (+ path on level 2).
type versionPickerRow struct {
	appID     string
	path      string
	text      string
	drillable bool
	romID     *int
	isCurrent bool
}

// pickerLevel is one list the user is looking at.
type pickerLevel struct {
	title         string
	rows          []versionPickerRow
	selectedIndex int
	visibleStart  int
	hasBehind     bool // a versions list to go back to
}

func (s *VersionPickerScreen) Draw(input VersionPickerInput) (VersionPickerOutput, error) {
	logger := gaba.GetLogger()
	output := VersionPickerOutput{Action: VersionPickerActionBack, Platform: input.Platform, Game: input.Game}

	client := romm.NewClientFromHost(input.Host, input.Config.ApiTimeout.Duration())

	versions := input.Versions
	if versions == nil {
		var fetchErr error
		gaba.ProcessMessage(
			i18n.Localize(&goi18n.Message{ID: "version_picker_loading", Other: "Loading versions..."}, nil),
			gaba.ProcessMessageOptions{ShowThemeBackground: true},
			func() (any, error) {
				versions, fetchErr = client.GetLiteBoxVersions(input.Game.ID)
				return nil, fetchErr
			},
		)
		if fetchErr != nil {
			logger.Warn("LiteBox versions fetch failed", "game", input.Game.Name, "error", fetchErr)
			showVersionPickerError()
			return output, nil
		}
	}

	if len(versions) == 0 {
		showVersionPickerError()
		return output, nil
	}

	top := pickerLevel{title: input.Game.Name, rows: s.versionRows(input, versions)}
	top.selectedIndex = currentRowIndex(top.rows)

	var current pickerLevel
	if len(versions) == 1 && versions[0].Eligible {
		// One version and it is an archive to pick from: level 1 would be a single line the user
		// has to press for no reason, so open its roms directly.
		roms, ok := s.loadRoms(client, input, top.rows[0], false)
		if !ok {
			return output, nil
		}
		current = roms
	} else {
		current = top
	}

	for {
		options := gaba.DefaultListOptions(current.title, s.menuItems(current.rows))
		options.UseSmallTitle = true
		options.SelectedIndex = current.selectedIndex
		options.VisibleStartIndex = current.visibleStart
		options.StatusBar = StatusBar()
		options.ActionButton = buttons.VirtualButtonX
		options.FooterHelpItems = []gaba.FooterHelpItem{
			FooterBack(),
			{ButtonName: "A", HelpText: i18n.Localize(&goi18n.Message{ID: "version_picker_switch", Other: "Switch"}, nil)},
			{ButtonName: "X", HelpText: i18n.Localize(&goi18n.Message{ID: "version_picker_switch_delete", Other: "Switch & delete old"}, nil)},
		}

		sel, err := gaba.List(options)
		if err != nil {
			if !errors.Is(err, gaba.ErrCancelled) {
				logger.Error("Version picker list error", "error", err)
				return output, err
			}
			// B: one level back when there is one, otherwise leave without pinning anything.
			if current.hasBehind {
				current = top
				continue
			}
			return output, nil
		}

		deleteOld := false
		switch sel.Action {
		case gaba.ListActionSelected:
		case gaba.ListActionTriggered:
			deleteOld = true
		default:
			if current.hasBehind {
				current = top
				continue
			}
			return output, nil
		}

		if len(sel.Selected) == 0 || sel.Selected[0] < 0 || sel.Selected[0] >= len(current.rows) {
			continue
		}
		idx := sel.Selected[0]
		current.selectedIndex = idx
		current.visibleStart = max(0, idx-sel.VisiblePosition)
		if !current.hasBehind {
			top = current
		}

		row := current.rows[idx]
		if row.drillable {
			// An archive: nothing to pin at this level, whichever button was pressed.
			roms, ok := s.loadRoms(client, input, row, true)
			if ok {
				current = roms
			}
			continue
		}

		if newGame, switched := s.switchTo(client, input, row, deleteOld); switched {
			output.Action = VersionPickerActionSwitched
			output.Game = newGame
			return output, nil
		}
	}
}

// versionRows dresses level 1. gaba.MenuItem has no subtitle, so everything rides in the text:
// label, file name when it differs, size, rom count for an archive, and the markers.
func (s *VersionPickerScreen) versionRows(input VersionPickerInput, versions []romm.LiteBoxVersion) []versionPickerRow {
	ids := make([]int, 0, len(versions))
	for _, v := range versions {
		if v.RomID != nil {
			ids = append(ids, *v.RomID)
		}
	}
	local := s.localRomIDs(input.Config, ids)

	rows := make([]versionPickerRow, 0, len(versions))
	for _, v := range versions {
		var sb strings.Builder
		sb.WriteString(v.Label)
		if v.FileName != "" && v.FileName != v.Label {
			sb.WriteString(" - ")
			sb.WriteString(v.FileName)
		}
		if v.Eligible {
			if v.RomCount != nil {
				sb.WriteString(fmt.Sprintf(
					i18n.Localize(&goi18n.Message{ID: "version_picker_rom_count", Other: " (%d roms)"}, nil),
					*v.RomCount,
				))
			} else {
				sb.WriteString(i18n.Localize(&goi18n.Message{ID: "version_picker_archive", Other: " (archive)"}, nil))
			}
		} else if v.Size > 0 {
			sb.WriteString(" (" + formatByteSize(v.Size) + ")")
		}
		sb.WriteString(rowMarkers(v.IsCurrent, v.RomID != nil && local[*v.RomID], false, false, false))

		rows = append(rows, versionPickerRow{
			appID:     v.AppID,
			text:      sb.String(),
			drillable: v.Eligible,
			romID:     v.RomID,
			isCurrent: v.IsCurrent,
		})
	}
	return rows
}

// loadRoms opens level 2 for one eligible version. False when the server could not answer; the
// user then stays where they were.
func (s *VersionPickerScreen) loadRoms(client *romm.Client, input VersionPickerInput, version versionPickerRow, hasBehind bool) (pickerLevel, bool) {
	logger := gaba.GetLogger()

	var listing romm.LiteBoxRomsInVersion
	var fetchErr error
	gaba.ProcessMessage(
		i18n.Localize(&goi18n.Message{ID: "version_picker_loading_roms", Other: "Loading roms..."}, nil),
		gaba.ProcessMessageOptions{ShowThemeBackground: true},
		func() (any, error) {
			listing, fetchErr = client.GetLiteBoxRomsInVersion(input.Game.ID, version.appID)
			return nil, fetchErr
		},
	)
	if fetchErr != nil {
		logger.Warn("LiteBox roms fetch failed", "game", input.Game.Name, "appId", version.appID, "error", fetchErr)
		showVersionPickerError()
		return pickerLevel{}, false
	}

	ids := make([]int, 0, len(listing.Roms))
	for _, e := range listing.Roms {
		if e.RomID != nil {
			ids = append(ids, *e.RomID)
		}
	}
	local := s.localRomIDs(input.Config, ids)

	rows := make([]versionPickerRow, 0, len(listing.Roms))
	for _, e := range listing.Roms {
		var sb strings.Builder
		sb.WriteString(e.Label)
		if e.Size > 0 {
			sb.WriteString(" (" + formatByteSize(e.Size) + ")")
		}
		sb.WriteString(rowMarkers(e.IsCurrent, e.RomID != nil && local[*e.RomID], e.IsFavorite, e.IsLastPlayed, e.HasRa))
		rows = append(rows, versionPickerRow{
			appID:     version.appID,
			path:      e.Path,
			text:      sb.String(),
			romID:     e.RomID,
			isCurrent: e.IsCurrent,
		})
	}

	title := listing.ArchiveFileName
	if title == "" {
		title = listing.VersionLabel
	}
	if title == "" {
		title = input.Game.Name
	}

	level := pickerLevel{title: title, rows: rows, hasBehind: hasBehind}
	level.selectedIndex = currentRowIndex(rows)
	return level, true
}

// switchTo pins the row, resyncs the platform so the cache holds the new rom_id, drops the retired
// row, and (deleteOld) removes the local files of the version served before. The pin holds server
// side whatever happens after it, so everything past step 1 is best-effort and logged. Returns the
// game to show next and whether a switch happened; false keeps the user on the picker.
func (s *VersionPickerScreen) switchTo(client *romm.Client, input VersionPickerInput, row versionPickerRow, deleteOld bool) (romm.Rom, bool) {
	logger := gaba.GetLogger()

	if deleteOld {
		_, err := gaba.ConfirmationMessage(
			fmt.Sprintf(
				i18n.Localize(&goi18n.Message{ID: "version_picker_delete_confirm", Other: "Switch version and delete the local files of %s?"}, nil),
				input.Game.FsName,
			),
			[]gaba.FooterHelpItem{
				FooterCancel(),
				{ButtonName: "X", HelpText: i18n.Localize(&goi18n.Message{ID: "button_confirm", Other: "Confirm"}, nil)},
			},
			gaba.MessageOptions{ConfirmButton: buttons.VirtualButtonX},
		)
		if err != nil {
			return input.Game, false
		}
	}

	var pinErr error
	newGame := input.Game
	deleted := 0
	gaba.ProcessMessage(
		i18n.Localize(&goi18n.Message{ID: "version_picker_switching", Other: "Switching version..."}, nil),
		gaba.ProcessMessageOptions{ShowThemeBackground: true},
		func() (any, error) {
			var pin romm.LiteBoxPinResponse
			pin, pinErr = client.PinLiteBoxVersion(input.Game.ID, row.appID, row.path)
			if pinErr != nil {
				return nil, pinErr
			}
			logger.Info("LiteBox version pinned", "game", input.Game.Name, "appId", row.appID, "path", row.path, "oldRomId", input.Game.ID, "newRomId", pin.RomID)

			cm := cache.GetCacheManager()
			if cm == nil {
				return nil, nil
			}
			if err := cm.RefreshPlatformGames(input.Platform); err != nil {
				logger.Warn("Post-pin platform refresh failed", "platform", input.Platform.Name, "error", err)
			}
			if pin.RomID > 0 && pin.RomID != input.Game.ID {
				// The server no longer serves the old id to this client; without this the stale
				// row would sit in the list until the next incremental sync's purge.
				if err := cm.DeleteGame(input.Game.ID); err != nil {
					logger.Warn("Could not drop retired rom from cache", "romId", input.Game.ID, "error", err)
				}
			}
			if pin.RomID > 0 {
				if games, err := cm.GetGamesByIDs([]int{pin.RomID}); err == nil && len(games) == 1 {
					newGame = games[0]
				} else {
					logger.Warn("New rom not found in cache after refresh", "romId", pin.RomID, "error", err)
				}
			}
			if deleteOld {
				deleted = deleteLocalFiles(input.Config, input.Game, newGame)
			}
			return nil, nil
		},
	)

	if pinErr != nil {
		logger.Warn("LiteBox pin failed", "game", input.Game.Name, "error", pinErr)
		gaba.ConfirmationMessage(
			i18n.Localize(&goi18n.Message{ID: "version_picker_switch_error", Other: "Could not switch version. Please try again."}, nil),
			ContinueFooter(),
			gaba.MessageOptions{},
		)
		return input.Game, false
	}

	if deleteOld {
		gaba.ConfirmationMessage(
			fmt.Sprintf(
				i18n.Localize(&goi18n.Message{ID: "version_picker_deleted", Other: "Deleted %d local file(s)."}, nil),
				deleted,
			),
			ContinueFooter(),
			gaba.MessageOptions{},
		)
	}
	return newGame, true
}

// localRomIDs says which of these rom_ids this device holds a downloaded file for: the cached row
// for that id, when it exists and IsDownloaded. Rows without a rom_id yet (never pinned by anyone)
// cannot be local, nothing was ever served under them.
func (s *VersionPickerScreen) localRomIDs(config *internal.Config, ids []int) map[int]bool {
	local := make(map[int]bool)
	cm := cache.GetCacheManager()
	if cm == nil || len(ids) == 0 {
		return local
	}
	games, err := cm.GetGamesByIDs(ids)
	if err != nil {
		return local
	}
	for i := range games {
		if games[i].IsDownloaded(config) {
			local[games[i].ID] = true
		}
	}
	return local
}

func (s *VersionPickerScreen) menuItems(rows []versionPickerRow) []gaba.MenuItem {
	items := make([]gaba.MenuItem, len(rows))
	for i, r := range rows {
		items[i] = gaba.MenuItem{Text: r.text, Metadata: i}
	}
	return items
}

// localFilePaths lists exactly what IsDownloaded/GetLocalPath look at for this row: the m3u of a
// multi-disc game, else each served file name under the platform's rom directory. A ROM grout
// unzipped after download lives under another name and is not covered, the same blind spot the
// Download/Redownload label already has.
func localFilePaths(config *internal.Config, game romm.Rom) []string {
	if game.PlatformFSSlug == "" {
		return nil
	}
	platform := romm.Platform{ID: game.PlatformID, FSSlug: game.PlatformFSSlug, Name: game.PlatformDisplayName}
	romDir := config.GetPlatformRomDirectory(platform)

	var paths []string
	if game.HasMultipleFiles {
		paths = append(paths, filepath.Join(romDir, game.FsNameNoExt+".m3u"))
	}
	for _, f := range game.Files {
		if f.FileName != "" {
			paths = append(paths, filepath.Join(romDir, f.FileName))
		}
	}
	return paths
}

// deleteLocalFiles removes the old version's local files, never one the NEW version would occupy
// (two versions can share a file name). Returns how many files went.
func deleteLocalFiles(config *internal.Config, oldGame, newGame romm.Rom) int {
	logger := gaba.GetLogger()
	keep := make(map[string]bool)
	if newGame.ID != oldGame.ID {
		for _, p := range localFilePaths(config, newGame) {
			keep[p] = true
		}
	}

	deleted := 0
	for _, p := range localFilePaths(config, oldGame) {
		if keep[p] || !fileutil.FileExists(p) {
			continue
		}
		if err := os.Remove(p); err != nil {
			logger.Warn("Could not delete old version file", "path", p, "error", err)
			continue
		}
		logger.Info("Deleted old version file", "path", p)
		deleted++
	}
	return deleted
}

// rowMarkers is the suffix that stands in for Argosy's icons: served today, downloaded here,
// favourite, last played, RetroAchievements.
func rowMarkers(isCurrent, isLocal, isFavorite, isLastPlayed, hasRa bool) string {
	var sb strings.Builder
	if isCurrent {
		sb.WriteString(" [*]")
	}
	if isLocal {
		sb.WriteString(" [dl]")
	}
	if isFavorite {
		sb.WriteString(" [fav]")
	}
	if isLastPlayed {
		sb.WriteString(" [recent]")
	}
	if hasRa {
		sb.WriteString(" [RA]")
	}
	return sb.String()
}

func currentRowIndex(rows []versionPickerRow) int {
	for i, r := range rows {
		if r.isCurrent {
			return i
		}
	}
	return 0
}

func formatByteSize(bytes int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case bytes >= gb:
		return fmt.Sprintf("%.1f GB", float64(bytes)/gb)
	case bytes >= mb:
		return fmt.Sprintf("%.1f MB", float64(bytes)/mb)
	case bytes >= kb:
		return fmt.Sprintf("%.0f KB", float64(bytes)/kb)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func showVersionPickerError() {
	gaba.ConfirmationMessage(
		i18n.Localize(&goi18n.Message{ID: "version_picker_load_error", Other: "Failed to load versions. Please try again later."}, nil),
		ContinueFooter(),
		gaba.MessageOptions{},
	)
}
