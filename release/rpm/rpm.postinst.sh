# $1 == 1 for initial installation.
# $1 == 2 for upgrades.

if [ $1 -eq 1 ] ; then
    # Normally, the mirage-relay package would request shutdown of
    # its service before uninstallation. Unfortunately, the
    # mirage-relay package we distributed doesn't have those
    # scriptlets. We definitely want relaynode to be stopped when
    # installing miraged though, so we blindly try to turn off
    # relaynode here.
    #
    # However, we also want this package installation to look like an
    # upgrade from relaynode! Therefore, if relaynode is currently
    # enabled, we want to also enable miraged. If relaynode is
    # currently running, we also want to start miraged.
    #
    # If there doesn't seem to be an active or enabled relaynode on
    # the system, we follow the RPM convention for package installs,
    # which is to not enable or start the service.
    relaynode_enabled=0
    relaynode_running=0
    if systemctl is-enabled mirage-relay.service >/dev/null 2>&1; then
        relaynode_enabled=1
    fi
    if systemctl is-active mirage-relay.service >/dev/null 2>&1; then
        relaynode_running=1
    fi

    systemctl --no-reload disable mirage-relay.service >/dev/null 2>&1 || :
    systemctl stop mirage-relay.service >/dev/null 2>&1 || :

    if [ $relaynode_enabled -eq 1 ]; then
        systemctl enable miraged.service >/dev/null 2>&1 || :
    else
        systemctl preset miraged.service >/dev/null 2>&1 || : 
    fi

    if [ $relaynode_running -eq 1 ]; then
        systemctl start miraged.service >/dev/null 2>&1 || :
    fi
fi 
