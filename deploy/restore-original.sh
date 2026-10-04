#!/bin/sh
# Undo install.sh: stop evse-controller and re-enable the original software.
set -eu

BACKUP=/var/backups/evse-original
KIOSK_USER=${KIOSK_USER:-btcpower}
AUTOSTART=/home/$KIOSK_USER/.config/autostart

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

systemctl stop evse-controller 2>/dev/null || true
systemctl disable evse-controller 2>/dev/null || true
for svc in apache2 mysql; do
    systemctl enable "$svc" 2>/dev/null || true
    systemctl start "$svc" 2>/dev/null || true
done
[ -f "$BACKUP/root.crontab" ] && crontab -u root "$BACKUP/root.crontab"
rm -f "$AUTOSTART/evse-kiosk.desktop"
[ -f "$BACKUP/chromium-browser.desktop" ] && cp "$BACKUP/chromium-browser.desktop" "$AUTOSTART/"
echo "Original software restored; reboot to start it."
