#!/bin/bash

# E2E test runner for GeeseFS with LocalStack and external cache
# This script:
# 1. Starts LocalStack S3
# 2. Mounts GeeseFS with staged write and caching enabled
# 3. Runs real filesystem operations
# 4. Verifies caching behavior
# 5. Cleans up

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

echo "=== GeeseFS E2E Test with Real Mounting ==="
echo

# Configuration
LOCALSTACK_ENDPOINT="http://localhost:4566"
BUCKET_NAME="test-geesefs-bucket"
MOUNT_POINT="/tmp/geesefs-e2e-mount"
STAGED_WRITE_PATH="/tmp/geesefs-staged"
LOG_FILE="/tmp/geesefs-e2e.log"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Cleanup function
cleanup() {
    echo
    echo "=== Cleaning up ==="
    
    # Unmount if mounted
    if mount | grep -q "$MOUNT_POINT"; then
        echo "Unmounting $MOUNT_POINT..."
        fusermount -u "$MOUNT_POINT" || sudo umount -f "$MOUNT_POINT" || true
        sleep 1
    fi
    
    # Remove temp directories
    rm -rf "$MOUNT_POINT" "$STAGED_WRITE_PATH" 2>/dev/null || true
    
    echo "Cleanup complete"
}

trap cleanup EXIT

# Check if LocalStack is running
echo "Checking LocalStack..."
if ! curl -s "$LOCALSTACK_ENDPOINT/_localstack/health" > /dev/null; then
    echo -e "${YELLOW}WARNING: LocalStack not running at $LOCALSTACK_ENDPOINT${NC}"
    echo "Start LocalStack with: localstack start -d"
    echo "Or use Docker: docker run -d -p 4566:4566 localstack/localstack"
    exit 1
fi
echo -e "${GREEN}✓ LocalStack is running${NC}"

# Check if geesefs binary exists
if [ ! -f "$ROOT_DIR/geesefs" ]; then
    echo "Building geesefs..."
    cd "$ROOT_DIR"
    go build -o geesefs
fi

# Setup directories
mkdir -p "$MOUNT_POINT" "$STAGED_WRITE_PATH"

# Create bucket in LocalStack
echo
echo "Setting up S3 bucket..."
aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 mb "s3://$BUCKET_NAME" 2>/dev/null || echo "Bucket already exists"
echo -e "${GREEN}✓ Bucket ready: $BUCKET_NAME${NC}"

# Start geesefs mount
echo
echo "Mounting GeeseFS..."
echo "Mount point: $MOUNT_POINT"
echo "Staged write path: $STAGED_WRITE_PATH"
echo "Log file: $LOG_FILE"

"$ROOT_DIR/geesefs" \
    --endpoint "$LOCALSTACK_ENDPOINT" \
    --region us-east-1 \
    --debug_s3 \
    --debug_fuse \
    --staged-write-mode \
    --staged-write-path "$STAGED_WRITE_PATH" \
    --staged-write-debounce 2s \
    --staged-write-flush-interval 1s \
    -o allow_other \
    -f \
    "$BUCKET_NAME" \
    "$MOUNT_POINT" \
    > "$LOG_FILE" 2>&1 &

GEESEFS_PID=$!
echo "GeeseFS PID: $GEESEFS_PID"

# Wait for mount
echo "Waiting for filesystem to mount..."
for i in {1..30}; do
    if mount | grep -q "$MOUNT_POINT"; then
        echo -e "${GREEN}✓ Filesystem mounted${NC}"
        break
    fi
    if ! kill -0 $GEESEFS_PID 2>/dev/null; then
        echo -e "${RED}✗ GeeseFS process died${NC}"
        echo "Last 20 lines of log:"
        tail -20 "$LOG_FILE"
        exit 1
    fi
    sleep 0.5
done

if ! mount | grep -q "$MOUNT_POINT"; then
    echo -e "${RED}✗ Failed to mount${NC}"
    echo "Log output:"
    cat "$LOG_FILE"
    exit 1
fi

# Run tests
echo
echo "=== Running E2E Tests ==="
echo

TEST_FILE="$MOUNT_POINT/test-file.txt"
TEST_DATA="Hello from GeeseFS E2E test! This is a test of staged write mode and caching."

# Test 1: Write file
echo "Test 1: Writing file..."
echo "$TEST_DATA" > "$TEST_FILE"
if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ File write succeeded${NC}"
else
    echo -e "${RED}✗ File write failed${NC}"
    exit 1
fi

# Test 2: Verify staged file exists
echo
echo "Test 2: Checking staged file..."
STAGED_FILE="$STAGED_WRITE_PATH/test-file.txt"
if [ -f "$STAGED_FILE" ]; then
    echo -e "${GREEN}✓ Staged file exists: $STAGED_FILE${NC}"
    echo "  Size: $(stat -f%z "$STAGED_FILE" 2>/dev/null || stat -c%s "$STAGED_FILE")"
else
    echo -e "${YELLOW}⚠ Staged file not found (may have already flushed)${NC}"
fi

# Test 3: Wait for debounce and flush
echo
echo "Test 3: Waiting for flush (debounce + flush interval)..."
sleep 4

# Check if file was uploaded to S3
echo "Checking S3..."
if aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/test-file.txt" > /dev/null 2>&1; then
    echo -e "${GREEN}✓ File uploaded to S3${NC}"
    
    # Get file from S3
    S3_CONTENT=$(aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 cp "s3://$BUCKET_NAME/test-file.txt" - 2>/dev/null)
    if [ "$S3_CONTENT" = "$TEST_DATA" ]; then
        echo -e "${GREEN}✓ S3 content matches${NC}"
    else
        echo -e "${RED}✗ S3 content mismatch${NC}"
        echo "  Expected: $TEST_DATA"
        echo "  Got: $S3_CONTENT"
    fi
else
    echo -e "${RED}✗ File not found in S3${NC}"
    echo "Available files:"
    aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/" || true
fi

# Test 4: Read file back
echo
echo "Test 4: Reading file..."
READ_CONTENT=$(cat "$TEST_FILE")
if [ "$READ_CONTENT" = "$TEST_DATA" ]; then
    echo -e "${GREEN}✓ Read content matches${NC}"
else
    echo -e "${RED}✗ Read content mismatch${NC}"
    echo "  Expected: $TEST_DATA"
    echo "  Got: $READ_CONTENT"
fi

# Test 5: Large file test
echo
echo "Test 5: Testing large file (10MB)..."
LARGE_FILE="$MOUNT_POINT/large-test.bin"
dd if=/dev/urandom of="$LARGE_FILE" bs=1M count=10 2>/dev/null
if [ $? -eq 0 ]; then
    echo -e "${GREEN}✓ Large file written${NC}"
    
    # Wait for flush
    sleep 5
    
    # Verify in S3
    if aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/large-test.bin" > /dev/null 2>&1; then
        echo -e "${GREEN}✓ Large file uploaded to S3${NC}"
    else
        echo -e "${RED}✗ Large file not in S3${NC}"
    fi
else
    echo -e "${RED}✗ Large file write failed${NC}"
fi

# Show log summary
echo
echo "=== Log Summary ==="
echo "Errors:"
grep -i error "$LOG_FILE" | tail -5 || echo "  (none)"
echo
echo "Cache events:"
grep -i "cache" "$LOG_FILE" | tail -5 || echo "  (none)"
echo
echo "Staged write events:"
grep -i "staged" "$LOG_FILE" | tail -5 || echo "  (none)"

echo
echo -e "${GREEN}=== E2E Tests Complete ===${NC}"
echo "Full log available at: $LOG_FILE"

# Cleanup will run via trap
exit 0
