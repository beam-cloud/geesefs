#!/bin/bash

# Integration test using PUBLIC API with FUSE mounting
# This test:
# - Uses MountFuse() public API
# - Mounts real FUSE filesystem
# - Uses LocalStack for S3
# - Passes mock cache via public API
# - Tests through mounted filesystem

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo "================================================================"
echo "   Integration Test - PUBLIC API + FUSE Mount"
echo "================================================================"
echo

# Check requirements
echo "Checking requirements..."

# Check LocalStack
if ! curl -sf http://localhost:4566/_localstack/health > /dev/null 2>&1; then
    echo -e "${RED}✗ LocalStack not running${NC}"
    echo "  Start with: localstack start -d"
    echo "  Or: docker run -d -p 4566:4566 localstack/localstack"
    exit 1
fi
echo -e "${GREEN}✓ LocalStack running${NC}"

# Check FUSE
if ! command -v fusermount &> /dev/null; then
    echo -e "${RED}✗ fusermount not found${NC}"
    echo "  Install with: sudo apt-get install fuse"
    exit 1
fi
echo -e "${GREEN}✓ FUSE available${NC}"

# Check AWS CLI
if ! command -v aws &> /dev/null; then
    echo -e "${RED}✗ AWS CLI not found${NC}"
    exit 1
fi
echo -e "${GREEN}✓ AWS CLI available${NC}"

echo

# Run test
echo "Running integration test..."
echo "This will:"
echo "  1. Mount filesystem using PUBLIC API (MountFuse)"
echo "  2. Pass mock cache via ExternalCacheClient"
echo "  3. Write files through mounted filesystem"
echo "  4. Read files back and verify"
echo "  5. Measure throughput"
echo "  6. Verify caching behavior"
echo

cd "$ROOT_DIR"

RUN_MOUNT_INTEGRATION=true go test -v ./test -run TestIntegrationWithMount -timeout 120s 2>&1 | tee /tmp/mount_test_output.txt

EXIT_CODE=${PIPESTATUS[0]}

echo
echo "================================================================"

if [ $EXIT_CODE -eq 0 ]; then
    echo -e "${GREEN}✓ ALL TESTS PASSED${NC}"
    echo
    echo "Test verified:"
    echo "  ✓ PUBLIC API (MountFuse) used"
    echo "  ✓ FUSE filesystem mounted"
    echo "  ✓ Mock cache passed via public API"
    echo "  ✓ Files written/read through mount"
    echo "  ✓ Throughput measured"
    echo "  ✓ Caching verified"
    echo "  ✓ Correctness verified"
else
    echo -e "${RED}✗ TESTS FAILED${NC}"
    echo "See output above for details"
fi

echo "================================================================"

exit $EXIT_CODE
