# Pixel Life

A pixel-art app for medicine reminders and saving money.

- `app/index.html` is the app itself. The desktop app loads updates of it from here.
- `app/PixelLife.exe` is the Windows desktop app. It checks `app/version.json` on start and every 3 hours, and updates itself.
- `src/` is the desktop app's Go source. Build with `./build.sh`.

Alarms only work while the PC is on and awake. Keep phone alarms as a backup.
