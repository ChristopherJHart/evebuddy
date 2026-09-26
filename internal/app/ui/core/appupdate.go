package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"

	"github.com/dustin/go-humanize"

	"github.com/ErikKalkoken/evebuddy/internal/app/ui"
	"github.com/ErikKalkoken/evebuddy/internal/github"
	"github.com/ErikKalkoken/evebuddy/internal/selfupdate"
	"github.com/ErikKalkoken/evebuddy/internal/xdesktop"
	"github.com/ErikKalkoken/evebuddy/internal/xwidget"
)

// CanSelfUpdate reports whether this installation can update itself in place.
// When it can not, the UI offers a link to the download page instead.
func (u *baseUI) CanSelfUpdate() bool {
	return u.su != nil
}

// ShowUpdateDialog shows the update dialog for an available update.
//
// When the installation can update itself, the dialog offers to download and
// apply the update, and then to restart. Otherwise it links to the download
// page. Must be called on the UI thread.
func (u *baseUI) ShowUpdateDialog(v github.VersionInfo) {
	content := container.New(
		layout.NewCustomPaddedVBoxLayout(0),
		container.NewHBox(
			widget.NewLabel("Latest version:"), layout.NewSpacer(), widget.NewLabel(v.Latest),
		),
		container.NewHBox(
			widget.NewLabel("You have:"), layout.NewSpacer(), widget.NewLabel(v.Local),
		),
	)
	if !u.CanSelfUpdate() {
		u.showDownloadPageDialog(content, v)
		return
	}
	// Planning is cheap and catches an unsupported or read-only installation
	// before offering an update that could not be applied.
	plan, err := u.su.Plan(v.Latest)
	if err != nil {
		slog.Warn("selfupdate: cannot update in place, offering download instead", "error", err)
		hint := widget.NewLabel(selfUpdateUnavailableReason(err))
		hint.Wrapping = fyne.TextWrapWord
		content.Add(xwidget.NewStandardSpacer())
		content.Add(hint)
		u.showDownloadPageDialog(content, v)
		return
	}
	content.Add(xwidget.NewStandardSpacer())
	var d dialog.Dialog
	d = dialog.NewCustomConfirm(
		"Update available", "Update & Restart", "Later", content, func(ok bool) {
			if !ok {
				return
			}
			u.runUpdate(plan)
		},
		u.MainWindow(),
	)
	xdesktop.DisableShortcutsForDialog(d, u.MainWindow())
	d.Show()
}

// showDownloadPageDialog shows an update dialog that can only link to the
// download page, which is the fallback whenever an update can not be applied
// in place.
func (u *baseUI) showDownloadPageDialog(content *fyne.Container, v github.VersionInfo) {
	url := ui.WebsiteRootURL().JoinPath("releases")
	d := dialog.NewCustomConfirm(
		"Update available", "Download", "Close", content, func(ok bool) {
			if !ok {
				return
			}
			if err := u.app.OpenURL(url); err != nil {
				ui.ShowErrorAndLog("Failed to open download page", err, u.IsDeveloperMode(), u.MainWindow())
			}
		},
		u.MainWindow(),
	)
	xdesktop.DisableShortcutsForDialog(d, u.MainWindow())
	d.Show()
}

// runUpdate downloads and applies an update, showing progress, and then offers
// to restart. Must be called on the UI thread.
func (u *baseUI) runUpdate(plan selfupdate.Plan) {
	bar := widget.NewProgressBar()
	bar.Min = 0
	bar.Max = 1
	status := widget.NewLabel("Starting download...")
	status.Wrapping = fyne.TextWrapWord

	ctx, cancel := context.WithCancel(context.Background())
	var canceled atomic.Bool
	d := dialog.NewCustomConfirm(
		"Updating EVE Buddy", "", "Cancel",
		container.New(layout.NewCustomPaddedVBoxLayout(0), status, bar),
		func(_ bool) {
			canceled.Store(true)
			cancel()
		},
		u.MainWindow(),
	)
	xdesktop.DisableShortcutsForDialog(d, u.MainWindow())
	d.Show()

	go func() {
		defer cancel()
		err := u.applyUpdate(ctx, plan, func(done, total int64) {
			fyne.Do(func() {
				if total > 0 {
					bar.SetValue(float64(done) / float64(total))
					status.SetText(fmt.Sprintf(
						"Downloading %s of %s...",
						humanize.Bytes(uint64(done)), humanize.Bytes(uint64(total)),
					))
				} else {
					status.SetText(fmt.Sprintf("Downloading %s...", humanize.Bytes(uint64(done))))
				}
			})
		}, func(text string) {
			fyne.Do(func() {
				status.SetText(text)
			})
		})
		fyne.Do(func() {
			d.Hide()
			if err != nil {
				// A cancel tears down the context, so the resulting error is
				// expected and must not be reported as a failure.
				if canceled.Load() || errors.Is(err, context.Canceled) {
					u.DisplaySnackbar("Update canceled")
					return
				}
				ui.ShowErrorAndLog("Failed to install update", err, u.IsDeveloperMode(), u.MainWindow())
				return
			}
			u.showRestartDialog(plan.Version)
		})
	}()
}

// applyUpdate performs the download and swap for an update. It runs off the UI
// thread; progress and setStatus are called from this goroutine.
func (u *baseUI) applyUpdate(
	ctx context.Context,
	plan selfupdate.Plan,
	progress func(done, total int64),
	setStatus func(string),
) error {
	// The release is fetched again rather than carried over from the version
	// check, because only the release carries the asset URL and its digest, and
	// the check may be up to an hour old by now.
	release, err := github.LatestRelease(ctx, githubOwner, githubRepo)
	if err != nil {
		return fmt.Errorf("fetch release info: %w", err)
	}
	if release.Version != plan.Version {
		return fmt.Errorf(
			"release changed while updating: expected %s, found %s; please try again",
			plan.Version, release.Version,
		)
	}
	asset, ok := release.Asset(plan.AssetName)
	if !ok {
		return fmt.Errorf("%w: %s in release %s", selfupdate.ErrAssetMissing, plan.AssetName, release.TagName)
	}
	staged, err := u.su.Download(ctx, plan, asset, progress)
	if err != nil {
		return err
	}
	// Discard is a no-op once Apply has consumed the staging dir, so this
	// cleans up only on a failure or cancel.
	defer staged.Discard()

	setStatus("Verifying and installing...")
	if err := u.su.Apply(ctx, staged); err != nil {
		return err
	}
	return nil
}

// showRestartDialog offers to restart the app after an update was applied.
// Must be called on the UI thread.
func (u *baseUI) showRestartDialog(version string) {
	message := widget.NewLabel(fmt.Sprintf(
		"EVE Buddy %s has been installed.\n\n"+
			"The new version will be used after a restart.", version,
	))
	message.Wrapping = fyne.TextWrapWord
	d := dialog.NewCustomConfirm(
		"Update installed", "Restart now", "Later", message, func(ok bool) {
			if !ok {
				u.DisplaySnackbar("Update will be used the next time you start EVE Buddy")
				return
			}
			// The relaunch itself happens after the app has shut down, since the
			// single instance lock is held until then.
			u.requestRelaunch()
			u.app.Quit()
		},
		u.MainWindow(),
	)
	xdesktop.DisableShortcutsForDialog(d, u.MainWindow())
	d.Show()
}

// selfUpdateUnavailableReason returns a message explaining why an update can not
// be applied in place, phrased for a user rather than a log file.
func selfUpdateUnavailableReason(err error) string {
	switch {
	case errors.Is(err, selfupdate.ErrNotWritable):
		return "EVE Buddy can not update itself because its folder is not writable. " +
			"Please download the new version manually, or move EVE Buddy to a folder you own."
	case errors.Is(err, selfupdate.ErrTransientLocation):
		return "EVE Buddy is running from a temporary folder, which happens when the .exe is " +
			"started directly from the downloaded ZIP file. An update would be lost. " +
			"Please unpack the ZIP file into a folder of your choice first, then run EVE Buddy from there."
	case errors.Is(err, selfupdate.ErrAssetMissing):
		return "No automatic update is available for this platform. Please download the new version manually."
	default:
		return "EVE Buddy can not update itself for this installation. Please download the new version manually."
	}
}
