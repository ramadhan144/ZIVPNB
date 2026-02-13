#!/bin/bash

# udpgw Uninstaller - Versi Alternatif

if [[ $EUID -ne 0 ]]; then
    echo "Error: Script ini harus dijalankan sebagai root (gunakan sudo)."
    exit 1
fi

echo "=== udpgw Uninstaller ==="
echo "Script ini akan menghapus:"
echo "   • Service systemd udpgw"
echo "   • Binary /usr/local/bin/udpgw"
echo "   • Folder konfigurasi /etc/udpgw"
echo

read -p "Lanjutkan penghapusan? (y/N): " confirm
if [[ ! "$confirm" =~ ^[Yy]$ ]]; then
    echo "Dibatalkan."
    exit 0
fi

echo
echo "Menghentikan dan menonaktifkan service..."
systemctl stop udpgw 2>/dev/null
systemctl disable udpgw 2>/dev/null
killall udpgw 2>/dev/null

echo "Menghapus file dan folder..."
rm -f /usr/local/bin/udpgw && echo "   → /usr/local/bin/udpgw dihapus"
rm -rf /etc/udpgw && echo "   → /etc/udpgw dihapus"
rm -f /etc/systemd/system/udpgw.service && echo "   → Service file dihapus"

echo "Reload systemd daemon..."
systemctl daemon-reload
systemctl daemon-reexec

echo
echo "Uninstall selesai!"
echo "udpgw telah dihapus sepenuhnya dari sistem."
