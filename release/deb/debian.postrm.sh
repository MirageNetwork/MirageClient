#!/bin/sh
set -e
if [ -d /run/systemd/system ] ; then
	systemctl --system daemon-reload >/dev/null || true
fi

if [ -x "/usr/bin/deb-systemd-helper" ]; then
    if [ "$1" = "remove" ]; then
		deb-systemd-helper mask 'miraged.service' >/dev/null || true
	fi

    if [ "$1" = "purge" ]; then
		deb-systemd-helper purge 'miraged.service' >/dev/null || true
		deb-systemd-helper unmask 'miraged.service' >/dev/null || true
		rm -rf /var/lib/mirage
	fi
fi
