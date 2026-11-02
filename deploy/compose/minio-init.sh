#!/bin/sh
# One-shot initialiser of the LAB MinIO (compose profile "lab"). Idempotent: run it
# again and it converges. It creates:
#
#   bucket   $BUCKET, with VERSIONING ENABLED (see below)
#   policy   cryo-gateway   get, put and list under $PREFIX, and delete ONLY under
#                           (s3:GetBucketLocation sits in its own statement: the s3:prefix
#                           condition is valid for s3:ListBucket only, and MinIO rejects a
#                           policy that attaches it to GetBucketLocation - FIX15, lab run)
#                           $PREFIX/<device>/locks/ (restic removes its own lock files;
#                           engine 3.4). It holds none of s3:DeleteObjectVersion,
#                           s3:PutLifecycleConfiguration, s3:PutBucketVersioning or
#                           s3:BypassGovernanceRetention.
#   policy   cryo-admin     every s3 action on the bucket (the maintenance worker: forget,
#                           prune, overwrite)
#   users    $LAB_GATEWAY_ACCESS_KEY (cryo-gateway) and $LAB_ADMIN_ACCESS_KEY (cryo-admin)
#
# Storage-layer protection is versioning, not the gateway's flags. rclone's --append-only
# is an application-layer guard: the gateway key's s3:PutObject can overwrite an object if
# the gateway host is compromised. With versioning on, an overwrite or a delete marker only
# adds a version, and the gateway cannot remove a version (no s3:DeleteObjectVersion), turn
# versioning off or shorten a lifecycle rule. So a compromised gateway can write garbage
# and hide the current object, but the prior versions survive and are recoverable with the
# admin credential. Bucket versioning is REQUIRED in production too (deploy/compose/README.md).
# This is not immutability: Object Lock governance is M9 (L-GOV-1). Only the admin credential
# may hold lifecycle and version-delete rights.
#
# Production does not run this: you scope the provider keys yourself, and this file is
# the template for the policy. Nothing printed here is a secret.
#
# No secret is placed on a command line, with ONE exception that mc forces: creating a user
# is `mc admin user add ALIAS ACCESSKEY SECRETKEY`, so the two LAB user secrets are in the
# argv of that short-lived mc process, inside this single-purpose container. They are
# throw-away lab values; do not reuse them anywhere.

set -eu

for v in MINIO_ROOT_USER MINIO_ROOT_PASSWORD BUCKET LAB_GATEWAY_ACCESS_KEY LAB_GATEWAY_SECRET_KEY \
         LAB_ADMIN_ACCESS_KEY LAB_ADMIN_SECRET_KEY; do
  eval "val=\${$v:-}"
  if [ -z "$val" ]; then
    echo "minio-init: $v is not set" >&2
    exit 1
  fi
done
PREFIX="${PREFIX:-}"
PREFIX="${PREFIX#/}"
PREFIX="${PREFIX%/}"

SERVER="${MINIO_SERVER:-http://minio:9000}"
ALIAS=lab

# The root credentials reach mc through its environment (MC_HOST_<alias>), never on a
# command line. They must be URL-safe (letters, digits, - _ .).
export MC_HOST_lab="${SERVER%%://*}://$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD@${SERVER#*://}"

echo "minio-init: waiting for the server"
i=0
until mc ls "$ALIAS" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 30 ]; then
    echo "minio-init: the server did not accept the root credentials" >&2
    exit 1
  fi
  sleep 2
done

echo "minio-init: bucket $BUCKET"
mc mb --ignore-existing "$ALIAS/$BUCKET" >/dev/null

# Versioning is what makes an overwrite by the gateway key recoverable. Fail loudly if it
# cannot be enabled: a lab that silently lacks it would test something production forbids.
echo "minio-init: versioning on $BUCKET"
mc version enable "$ALIAS/$BUCKET" >/dev/null

if [ -n "$PREFIX" ]; then
  # Both the objects under the prefix AND the bare prefix key (FIX16, lab run): rclone probes
  # whether its remote root is a file with a HEAD on "<prefix>"; a 403 there makes it treat
  # the prefix as a file and serve from the BUCKET ROOT, after which every object access is
  # outside the prefix (403 -> gateway 500). A 404 keeps the root on the prefix. No object is
  # ever stored at the bare key, so the grant is inert.
  OBJECTS="arn:aws:s3:::$BUCKET/$PREFIX\", \"arn:aws:s3:::$BUCKET/$PREFIX/*"
  LOCKS="arn:aws:s3:::$BUCKET/$PREFIX/*/locks/*"
  LISTPFX="\"$PREFIX\", \"$PREFIX/*\""
else
  OBJECTS="arn:aws:s3:::$BUCKET/*"
  LOCKS="arn:aws:s3:::$BUCKET/*/locks/*"
  LISTPFX="\"*\""
fi

cat > /tmp/cryo-gateway.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetBucketLocation"],
      "Resource": ["arn:aws:s3:::$BUCKET"]
    },
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": ["arn:aws:s3:::$BUCKET"],
      "Condition": {"StringLike": {"s3:prefix": [$LISTPFX]}}
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": ["$OBJECTS"]
    },
    {
      "Effect": "Allow",
      "Action": ["s3:DeleteObject"],
      "Resource": ["$LOCKS"]
    }
  ]
}
EOF

cat > /tmp/cryo-admin.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:*"],
      "Resource": ["arn:aws:s3:::$BUCKET", "arn:aws:s3:::$BUCKET/*"]
    }
  ]
}
EOF

echo "minio-init: policies"
mc admin policy create "$ALIAS" cryo-gateway /tmp/cryo-gateway.json >/dev/null
mc admin policy create "$ALIAS" cryo-admin /tmp/cryo-admin.json >/dev/null

# attach POLICY USER: attaching twice is an error in mc, so accept it when the user already has it.
attach() {
  mc admin policy attach "$ALIAS" "$1" --user "$2" >/dev/null 2>&1 \
    || mc admin user info "$ALIAS" "$2" 2>/dev/null | grep -q "$1"
}

echo "minio-init: users"
mc admin user add "$ALIAS" "$LAB_GATEWAY_ACCESS_KEY" "$LAB_GATEWAY_SECRET_KEY" >/dev/null
attach cryo-gateway "$LAB_GATEWAY_ACCESS_KEY"
mc admin user add "$ALIAS" "$LAB_ADMIN_ACCESS_KEY" "$LAB_ADMIN_SECRET_KEY" >/dev/null
attach cryo-admin "$LAB_ADMIN_ACCESS_KEY"

# Lifecycle (OPTIONAL, left off in the lab so versions accumulate and can be inspected).
# Expire NON-CURRENT versions only after N days, applied with the ADMIN credential, never
# the gateway's (the gateway policy has no s3:PutLifecycleConfiguration). Pick N larger than
# the window in which you would notice tampering and than your provider's minimum storage
# duration. The current version is never expired by this rule. To apply it by hand:
#
#   export MC_HOST_labadmin="http://$LAB_ADMIN_ACCESS_KEY:$LAB_ADMIN_SECRET_KEY@minio:9000"
#   mc ilm rule add --noncurrent-expire-days 30 labadmin/$BUCKET
#
# (mc ilm rule add is not idempotent: list with `mc ilm rule ls` first.)

rm -f /tmp/cryo-gateway.json /tmp/cryo-admin.json
touch /tmp/minio-init.done
echo "minio-init: done (bucket $BUCKET, prefix '${PREFIX}', users $LAB_GATEWAY_ACCESS_KEY and $LAB_ADMIN_ACCESS_KEY)"
