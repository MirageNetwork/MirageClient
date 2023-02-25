# $1 == 0 for uninstallation.
# $1 == 1 for removing old package during upgrade.

if [ $1 -eq 0 ] ; then 
        # Package removal, not upgrade 
        systemctl --no-reload disable tailscaled.service > /dev/null 2>&1 || : 
        systemctl stop tailscaled.service > /dev/null 2>&1 || : 
fi
