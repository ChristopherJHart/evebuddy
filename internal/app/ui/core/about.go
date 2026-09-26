package core

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/icrowley/fake"

	"github.com/ErikKalkoken/evebuddy/internal/app/ui"
	"github.com/ErikKalkoken/evebuddy/internal/github"
	"github.com/ErikKalkoken/evebuddy/internal/xwidget"
)

const (
	discordServerURL = "https://discord.gg/tVSCQEVJnJ"
)

func makeAboutPage(u *baseUI) fyne.CanvasObject {
	title := widget.NewLabel(ui.Name())
	title.SizeName = theme.SizeNameSubHeadingText
	title.TextStyle.Bold = true

	v, err := github.NormalizeVersion(u.app.Metadata().Version)
	if err != nil {
		slog.Error("normalize local version", "error", err)
		v = "?"
	}
	currentVersion := widget.NewLabel(v)
	rootURL := ui.WebsiteRootURL()
	releaseNotes := widget.NewHyperlink("What's new", rootURL.JoinPath("releases", "v"+v))

	_, size := u.MainWindow().Canvas().InteractiveArea()
	x := fmt.Sprintf("%d x %d", int(size.Width), int(size.Height))
	techInfos := container.New(layout.NewCustomPaddedVBoxLayout(0),
		container.NewHBox(widget.NewLabel("Main window size:"), layout.NewSpacer(), widget.NewLabel(x)),
	)
	showSnackbar := widget.NewButton("Show Snackbar (debug)", func() {
		u.DisplaySnackbar(fake.Paragraph())
	})
	dummyInput := widget.NewEntry()
	dummyInput.PlaceHolder = "Enter sb to fire a snackbar"
	var fired bool
	dummyInput.OnChanged = func(s string) {
		if s == "sb" && !fired {
			fired = true
			u.DisplaySnackbar(fake.Paragraph())
		}
		if s == "" {
			fired = false
		}
	}
	if !u.IsDeveloperMode() {
		techInfos.Hide()
		showSnackbar.Hide()
		dummyInput.Hide()
	}
	discordURL, _ := url.Parse(discordServerURL)
	support := widget.NewLabel("For support please open an issue on our web site or join our Discord server.")
	support.Wrapping = fyne.TextWrapWord

	// The label is chosen for what the link actually does: install the update
	// in place, or send the user to the download page.
	updateLinkLabel := "Download"
	if u.CanSelfUpdate() {
		updateLinkLabel = "Update now"
	}
	var updateVersion github.VersionInfo
	updateAvailableLink := xwidget.NewCustomHyperlink(updateLinkLabel, func() {
		u.ShowUpdateDialog(updateVersion)
	})
	updateAvailableRow := container.NewHBox(
		widget.NewLabelWithStyle("Update available", fyne.TextAlignLeading, fyne.TextStyle{
			Bold: true,
		}),
		updateAvailableLink,
	)
	updateAvailableRow.Hide()
	go func() {
		v, err := u.availableUpdate(context.Background())
		if err != nil {
			slog.Error("Failed to fetch available updates")
			return
		}
		if !v.IsRemoteNewer {
			return
		}
		fyne.Do(func() {
			updateVersion = v
			updateAvailableRow.Show()
		})
	}()
	c := container.New(
		layout.NewCustomPaddedVBoxLayout(0),
		title,
		container.NewHBox(currentVersion, releaseNotes),
		updateAvailableRow,
		techInfos,
		showSnackbar,
		dummyInput,
		support,
		container.NewHBox(
			widget.NewHyperlink("Website", rootURL),
			widget.NewHyperlink("Downloads", rootURL.JoinPath("releases")),
			widget.NewHyperlink("Discord", discordURL),
		),
		widget.NewLabel("\"EVE\", \"EVE Online\", \"CCP\", \nand all related logos and images \nare trademarks or registered trademarks of CCP hf."),
		widget.NewLabel("(c) 2024-26 Erik Kalkoken"),
	)
	return c
}
