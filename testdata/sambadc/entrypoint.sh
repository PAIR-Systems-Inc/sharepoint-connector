#!/bin/bash
set -e
REALM="${REALM:-CORP.EXAMPLE.TEST}"
DOMAIN="${DOMAIN:-CORP}"
ADMINPASS="${ADMINPASS:-Adm1n!Passw0rd2026}"
SVCUSER="${SVCUSER:-svc-goodmem}"
SVCPASS="${SVCPASS:-Svc!Passw0rd2026}"

if [ ! -f /var/lib/samba/private/sam.ldb ]; then
  echo "[provision] creating AD domain $REALM ..."
  rm -f /etc/samba/smb.conf
  samba-tool domain provision \
      --use-rfc2307 --realm="$REALM" --domain="$DOMAIN" \
      --server-role=dc --dns-backend=SAMBA_INTERNAL \
      --adminpass="$ADMINPASS" --host-name=dc

  echo "[provision] creating service account $SVCUSER ..."
  samba-tool user create "$SVCUSER" "$SVCPASS"

  mkdir -p /srv/share
  cat >> /etc/samba/smb.conf <<SMBEOF

[data]
    path = /srv/share
    read only = yes
    valid users = $SVCUSER
SMBEOF

  echo "[provision] exporting keytab ..."
  samba-tool domain exportkeytab /keytab/svc.keytab --principal="$SVCUSER@$REALM"
  chmod 644 /keytab/svc.keytab
  echo "[provision] done"
fi

echo "[run] starting samba (AD DC) ..."
exec samba -i -s /etc/samba/smb.conf
