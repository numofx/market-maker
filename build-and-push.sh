#!/usr/bin/env bash
# Build the mm-bot image and push it to ECR, tagged by git SHA.
#
# The same convention as numofx/exchange infra/aws/build-and-push.sh: the tag is the commit and
# nothing else, and the ECR repository is IMMUTABLE, so a task definition names an image that traces
# back to exactly one tree. df66d7b952ac (2026-10-13) was built by hand with these commands; this is
# them, so the next one is built the same way.
#
#   ./build-and-push.sh                 # builds HEAD
#   PROFILE=numo REGION=us-east-1 ./build-and-push.sh
set -euo pipefail

PROFILE="${PROFILE:-numo}"
REGION="${REGION:-us-east-1}"
REPOSITORY="${REPOSITORY:-numo-exchange/market-maker}"

cd "$(dirname "$0")"

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "refusing to build: the working tree is dirty, so the SHA would not describe the image" >&2
  exit 1
fi

GIT_SHA="$(git rev-parse --short=12 HEAD)"
ACCOUNT="$(aws sts get-caller-identity --profile "$PROFILE" --query Account --output text)"
REGISTRY="${ACCOUNT}.dkr.ecr.${REGION}.amazonaws.com"
IMAGE="${REGISTRY}/${REPOSITORY}:${GIT_SHA}"

echo "==> building ${GIT_SHA} into ${IMAGE}"

aws ecr get-login-password --region "$REGION" --profile "$PROFILE" \
  | docker login --username AWS --password-stdin "$REGISTRY"

# Fargate runs amd64; the build context is the repository root (see Dockerfile).
docker build \
  --platform linux/amd64 \
  --build-arg "GIT_SHA=${GIT_SHA}" \
  -t "$IMAGE" \
  .

docker push "$IMAGE"

cat <<EOF

pushed ${IMAGE}

Pin it in numofx/exchange infra/aws: image_market_maker (spot), image_market_maker_perp or
image_market_maker_future, whichever this build is for. Each is pinned separately, so building
does not deploy anything. Then plan (expect only that service's task definition and service),
apply, wait for the old task to stop (infra/aws/wait-for-rollout.sh), and commit the pin.
EOF
