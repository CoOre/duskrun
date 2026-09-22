#!/bin/sh
# atmoz/sftp runs /etc/sftp.d/*.sh as root before sshd starts. A named volume is
# created root-owned, so without this the SFTP user cannot write into its own
# backup directory and every upload fails with "permission denied".
set -eu
user="${SFTP_USER:-backup}"
mkdir -p "/home/$user/backup"
# The user's primary group is `users`, not one named after them: chown by
# user alone, or this fails with "unknown user/group".
chown "$user" "/home/$user/backup"
chmod 0750 "/home/$user/backup"
