package ui

import (
	"errors"
	"fmt"
	"grout/internal"
	"grout/romm"

	gaba "github.com/BrandonKowalski/gabagool/v2/pkg/gabagool"
	"github.com/BrandonKowalski/gabagool/v2/pkg/gabagool/i18n"
	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
)

type GameOptionsInput struct {
	Config   *internal.Config
	Host     romm.Host
	Platform romm.Platform
	Game     romm.Rom
}

type GameOptionsOutput struct {
	Action      GameOptionsAction
	Config      *internal.Config
	Host        romm.Host
	Platform    romm.Platform
	Game        romm.Rom
	NewSlotName string // Set when a new slot is created (for targeted upload)
	// Versions is filled for GameOptionsActionVersions: the list already fetched to decide whether
	// the entry shows at all, handed to the picker so it does not ask the server twice.
	Versions []romm.LiteBoxVersion
}

type GameOptionsScreen struct{}

func NewGameOptionsScreen() *GameOptionsScreen {
	return &GameOptionsScreen{}
}

func (s *GameOptionsScreen) Draw(input GameOptionsInput) (GameOptionsOutput, error) {
	config := input.Config
	output := GameOptionsOutput{Action: GameOptionsActionBack, Config: config, Host: input.Host, Platform: input.Platform, Game: input.Game}

	// Fetch save summary to determine available slots, and, on a LiteBox server, the versions the
	// game could be served as. The capabilities probe behind SupportsLiteBoxVersionSwitch is one
	// request per server for the life of the process (a stock RomM answers 404 once, then nothing).
	var slotNames []string
	var versions []romm.LiteBoxVersion
	client := romm.NewClientFromHost(input.Host, config.ApiTimeout.Duration())
	gaba.ProcessMessage(
		i18n.Localize(&goi18n.Message{ID: "synced_games_loading_detail", Other: "Loading save details..."}, nil),
		gaba.ProcessMessageOptions{ShowThemeBackground: true},
		func() (any, error) {
			if input.Host.DeviceID != "" {
				summary, err := client.GetSaveSummary(input.Game.ID)
				if err == nil {
					for _, slot := range summary.Slots {
						name := "autosave"
						if slot.Slot != nil {
							name = *slot.Slot
						}
						slotNames = append(slotNames, name)
					}
				}
			}
			if client.SupportsLiteBoxVersionSwitch() {
				if v, err := client.GetLiteBoxVersions(input.Game.ID); err == nil {
					versions = v
				} else {
					gaba.GetLogger().Warn("LiteBox versions fetch failed", "game", input.Game.Name, "error", err)
				}
			}
			return nil, nil
		},
	)

	oldSlotPref := config.GetSlotPreference(input.Game.ID)

	items := s.buildMenuItems(config, input.Game, input.Host.DeviceID != "", slotNames)

	showQRText := i18n.Localize(&goi18n.Message{ID: "game_options_show_qr", Other: "Show QR Code"}, nil)
	items = append(items, gaba.ItemWithOptions{
		Item:           gaba.MenuItem{Text: showQRText},
		Options:        []gaba.Option{{DisplayName: "", Value: "show_qr", Type: gaba.OptionTypeClickable}},
		SelectedOption: 0,
	})

	// LiteBox only (RommLiteBoxApi.cs): worth an entry when there is something to choose, i.e.
	// several versions, or a single eligible archive whose roms are the choice (the picker then
	// opens straight on them). One version served whole has nothing to switch to. Last on purpose,
	// below the stock entries.
	versionsText := ""
	if hasVersionChoice(versions) {
		versionsText = fmt.Sprintf(
			i18n.Localize(&goi18n.Message{ID: "game_options_versions", Other: "Versions (%d)"}, nil),
			versionChoiceCount(versions),
		)
		items = append(items, gaba.ItemWithOptions{
			Item:           gaba.MenuItem{Text: versionsText},
			Options:        []gaba.Option{{DisplayName: "", Value: "versions", Type: gaba.OptionTypeClickable}},
			SelectedOption: 0,
		})
	}

	title := i18n.Localize(&goi18n.Message{ID: "game_options_title", Other: "Game Options"}, nil)

	result, err := gaba.OptionsList(
		title,
		gaba.OptionListSettings{
			FooterHelpItems:      OptionsListFooter(),
			InitialSelectedIndex: 0,
			StatusBar:            StatusBar(),
			UseSmallTitle:        true,
		},
		items,
	)

	if err != nil {
		if errors.Is(err, gaba.ErrCancelled) {
			return output, nil
		}
		gaba.GetLogger().Error("Game options screen error", "error", err)
		return output, err
	}

	if result.Action == gaba.ListActionSelected {
		if result.Selected >= 0 && result.Selected < len(result.Items) {
			selectedItem := result.Items[result.Selected]
			if selectedItem.Item.Text == showQRText {
				output.Action = GameOptionsActionShowQR
				return output, nil
			}
			if versionsText != "" && selectedItem.Item.Text == versionsText {
				output.Action = GameOptionsActionVersions
				output.Versions = versions
				return output, nil
			}
		}
	}

	s.applySettings(config, input.Game, result.Items)

	if err = internal.SaveSlotPreferences(config); err != nil {
		gaba.GetLogger().Error("Error saving slot preferences", "error", err)
		return output, err
	}

	newSlotPref := config.GetSlotPreference(input.Game.ID)
	if newSlotPref != oldSlotPref {
		output.Action = GameOptionsActionSyncNow
		// Check if this is a brand-new slot (not on server yet) for targeted upload
		isNewSlot := true
		for _, name := range slotNames {
			if name == newSlotPref {
				isNewSlot = false
				break
			}
		}
		if isNewSlot {
			output.NewSlotName = newSlotPref
		}
	} else {
		output.Action = GameOptionsActionSaved
	}
	return output, nil
}

// hasVersionChoice is Argosy's rule (GameDetailViewModel.refreshLiteBoxVersionsInBackground).
func hasVersionChoice(versions []romm.LiteBoxVersion) bool {
	if len(versions) > 1 {
		return true
	}
	for _, v := range versions {
		if v.Eligible {
			return true
		}
	}
	return false
}

// versionChoiceCount: the number of versions, or, for a lone eligible archive, its rom count when
// the server knows it.
func versionChoiceCount(versions []romm.LiteBoxVersion) int {
	if len(versions) == 1 && versions[0].Eligible && versions[0].RomCount != nil {
		return *versions[0].RomCount
	}
	return len(versions)
}

func (s *GameOptionsScreen) buildMenuItems(config *internal.Config, game romm.Rom, deviceRegistered bool, slotNames []string) []gaba.ItemWithOptions {
	items := make([]gaba.ItemWithOptions, 0)

	if deviceRegistered {
		saveSlotText := i18n.Localize(&goi18n.Message{ID: "game_options_save_slot", Other: "Save Slot"}, nil)
		slotOpts := BuildSlotOptions(config, game.ID, slotNames)

		items = append(items, gaba.ItemWithOptions{
			Item:           gaba.MenuItem{Text: saveSlotText},
			Options:        slotOpts.Options,
			SelectedOption: slotOpts.SelectedIdx,
		})
	}

	return items
}

func (s *GameOptionsScreen) applySettings(config *internal.Config, game romm.Rom, items []gaba.ItemWithOptions) {
	saveSlotText := i18n.Localize(&goi18n.Message{ID: "game_options_save_slot", Other: "Save Slot"}, nil)

	for _, item := range items {
		if item.Item.Text == saveSlotText {
			if item.SelectedOption >= 0 && item.SelectedOption < len(item.Options) {
				selectedOpt := item.Options[item.SelectedOption]
				// Empty string values come from the "New Slot..." keyboard option
				// when the user dismisses the keyboard without typing. Intentionally
				// treated as a no-op so the preference remains unchanged.
				if selectedSlot, ok := selectedOpt.Value.(string); ok && selectedSlot != "" {
					config.SetSlotPreference(game.ID, selectedSlot)
				}
			}
		}
	}
}
