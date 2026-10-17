#!/bin/sh
# Install evse-controller on the EVSE and disable the original BTC Power
# software. Run as root from a directory containing evsectl and deploy/.
#
# The original software is disabled, not deleted, and a backup of root's
# crontab is kept, so it can be restored with deploy/restore-original.sh.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-$HERE/../evsectl}
BACKUP=/var/backups/evse-original
KIOSK_USER=${KIOSK_USER:-btcpower}

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }
[ -x "$BIN" ] || { echo "evsectl binary not found at $BIN (set BIN=...)" >&2; exit 1; }

echo "== Backing up original configuration to $BACKUP"
mkdir -p "$BACKUP"
if crontab -l -u root >/dev/null 2>&1 && [ ! -f "$BACKUP/root.crontab" ]; then
    crontab -l -u root > "$BACKUP/root.crontab"
fi
AUTOSTART=/home/$KIOSK_USER/.config/autostart
if [ -f "$AUTOSTART/chromium-browser.desktop" ]; then
    cp -n "$AUTOSTART/chromium-browser.desktop" "$BACKUP/" || true
fi

echo "== Stopping original software"
# Root's crontab starts the serial daemons, OCPP scripts and remote access.
crontab -r -u root 2>/dev/null || true
for p in uartCharger1 uartCharger2 uartPowerMeter 680HIDMSR writeocppnew2 samplesocpp2 bootScript2; do
    pkill -f "$p" 2>/dev/null || true
done
pkill -f 'Nayax.jar' 2>/dev/null || true
for svc in apache2 mysql; do
    systemctl stop "$svc" 2>/dev/null || true
    systemctl disable "$svc" 2>/dev/null || true
done

echo "== Installing evse-controller"
id evse >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin evse
usermod -a -G dialout evse
install -m 0755 "$BIN" /usr/local/bin/evsectl
install -d -m 0750 -o evse -g evse /etc/evse-controller
if [ ! -f /etc/evse-controller/config.yaml ]; then
    install -m 0600 -o evse -g evse "$HERE/config.example.yaml" /etc/evse-controller/config.yaml
    echo "   Created /etc/evse-controller/config.yaml -- set admin_password before use."
fi
install -d -m 0755 -o evse -g evse /var/lib/evse-controller
install -d -m 0755 -o evse -g evse /var/log/evse-controller
install -m 0644 "$HERE/evse-controller.service" /etc/systemd/system/evse-controller.service
systemctl daemon-reload
systemctl enable evse-controller
systemctl restart evse-controller

echo "== Pointing the kiosk browser at the new UI"
if [ -d "/home/$KIOSK_USER" ]; then
    install -d -o "$KIOSK_USER" -g "$KIOSK_USER" "$AUTOSTART"
    rm -f "$AUTOSTART/chromium-browser.desktop"
    install -m 0644 -o "$KIOSK_USER" -g "$KIOSK_USER" "$HERE/evse-kiosk.desktop" "$AUTOSTART/evse-kiosk.desktop"
fi

echo "== Done. Status: systemctl status evse-controller; UI: http://<host>:8080/"
echo "   Reboot to restart the kiosk browser."
