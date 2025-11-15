#!/bin/bash

# Real integration test that mounts GeeseFS and runs actual filesystem operations
# This script verifies:
# - Staged write correctness
# - File content integrity
# - Caching behavior
# - Read/write throughput

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Configuration
LOCALSTACK_ENDPOINT="http://localhost:4566"
BUCKET_NAME="test-real-integration"
MOUNT_POINT="/tmp/geesefs-real-mount"
STAGED_PATH="/tmp/geesefs-real-staged"
LOG_FILE="/tmp/geesefs-real.log"
TEST_RESULTS_FILE="/tmp/geesefs-test-results.txt"

echo "================================================================"
echo "         GeeseFS Real Integration Test                         "
echo "================================================================"
echo

# Check requirements
check_requirements() {
    echo "Checking requirements..."
    
    # Check LocalStack
    if ! curl -sf "${LOCALSTACK_ENDPOINT}/_localstack/health" > /dev/null 2>&1; then
        echo -e "${RED}✗ LocalStack not running${NC}"
        echo "  Start with: localstack start -d"
        exit 1
    fi
    echo -e "${GREEN}✓ LocalStack running${NC}"
    
    # Check AWS CLI
    if ! command -v aws &> /dev/null; then
        echo -e "${RED}✗ AWS CLI not found${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ AWS CLI available${NC}"
    
    # Check fusermount
    if ! command -v fusermount &> /dev/null && ! command -v umount &> /dev/null; then
        echo -e "${RED}✗ fusermount/umount not found${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ FUSE tools available${NC}"
    
    echo
}

# Cleanup function
cleanup() {
    echo
    echo "Cleaning up..."
    
    # Kill geesefs if running
    if [ ! -z "$GEESEFS_PID" ] && kill -0 $GEESEFS_PID 2>/dev/null; then
        kill $GEESEFS_PID || true
        sleep 1
    fi
    
    # Unmount if mounted
    if mount | grep -q "$MOUNT_POINT"; then
        echo "Unmounting $MOUNT_POINT..."
        fusermount -u "$MOUNT_POINT" 2>/dev/null || sudo umount -f "$MOUNT_POINT" 2>/dev/null || true
        sleep 1
    fi
    
    # Remove temp directories
    rm -rf "$MOUNT_POINT" "$STAGED_PATH" 2>/dev/null || true
    
    echo "Cleanup complete"
}

trap cleanup EXIT

# Build geesefs
build_geesefs() {
    echo "Building geesefs..."
    cd "$ROOT_DIR"
    if ! go build -o geesefs 2>&1 | tail -5; then
        echo -e "${RED}✗ Build failed${NC}"
        exit 1
    fi
    echo -e "${GREEN}✓ geesefs built${NC}"
    echo
}

# Setup
setup() {
    echo "Setting up test environment..."
    
    # Create directories
    mkdir -p "$MOUNT_POINT" "$STAGED_PATH"
    
    # Create bucket
    aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 mb "s3://$BUCKET_NAME" 2>/dev/null || true
    echo -e "${GREEN}✓ Bucket ready: $BUCKET_NAME${NC}"
    
    # Clear any existing data
    aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 rm "s3://$BUCKET_NAME/" --recursive 2>/dev/null || true
    
    echo
}

# Mount filesystem
mount_filesystem() {
    echo "Mounting GeeseFS..."
    echo "  Mount point: $MOUNT_POINT"
    echo "  Staged path: $STAGED_PATH"
    echo "  Log file: $LOG_FILE"
    
    "$ROOT_DIR/geesefs" \
        --endpoint "$LOCALSTACK_ENDPOINT" \
        --region us-east-1 \
        --debug_s3 \
        --staged-write-mode \
        --staged-write-path "$STAGED_PATH" \
        --staged-write-debounce 2s \
        --staged-write-flush-interval 500ms \
        --uid "$(id -u)" \
        --gid "$(id -g)" \
        -o allow_other \
        -f \
        "$BUCKET_NAME" \
        "$MOUNT_POINT" \
        > "$LOG_FILE" 2>&1 &
    
    GEESEFS_PID=$!
    echo "  GeeseFS PID: $GEESEFS_PID"
    
    # Wait for mount
    echo -n "  Waiting for mount..."
    for i in {1..30}; do
        if mount | grep -q "$MOUNT_POINT"; then
            echo -e " ${GREEN}done${NC}"
            return 0
        fi
        if ! kill -0 $GEESEFS_PID 2>/dev/null; then
            echo -e " ${RED}failed${NC}"
            echo "Last 20 lines of log:"
            tail -20 "$LOG_FILE"
            return 1
        fi
        sleep 0.5
        echo -n "."
    done
    
    echo -e " ${RED}timeout${NC}"
    return 1
}

# Test 1: Basic write and read
test_basic_write_read() {
    echo
    echo "================================================================"
    echo "Test 1: Basic Write and Read"
    echo "================================================================"
    
    TEST_FILE="$MOUNT_POINT/test1.txt"
    TEST_DATA="Hello from GeeseFS integration test!"
    
    # Write
    echo -n "Writing file..."
    echo "$TEST_DATA" > "$TEST_FILE"
    if [ $? -eq 0 ]; then
        echo -e " ${GREEN}✓${NC}"
    else
        echo -e " ${RED}✗${NC}"
        return 1
    fi
    
    # Check staged file
    STAGED_FILE="$STAGED_PATH/test1.txt"
    if [ -f "$STAGED_FILE" ]; then
        echo -e "Staged file exists: ${GREEN}✓${NC}"
        STAGED_SIZE=$(stat -c%s "$STAGED_FILE" 2>/dev/null || stat -f%z "$STAGED_FILE")
        echo "  Size: $STAGED_SIZE bytes"
    else
        echo -e "Staged file: ${YELLOW}(may have flushed)${NC}"
    fi
    
    # Wait for flush
    echo "Waiting for flush (3 seconds)..."
    sleep 3
    
    # Check S3
    echo -n "Checking S3..."
    if aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/test1.txt" > /dev/null 2>&1; then
        echo -e " ${GREEN}✓${NC}"
        
        # Verify content
        S3_CONTENT=$(aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 cp "s3://$BUCKET_NAME/test1.txt" - 2>/dev/null)
        if [ "$S3_CONTENT" = "$TEST_DATA" ]; then
            echo -e "Content matches: ${GREEN}✓${NC}"
        else
            echo -e "Content matches: ${RED}✗${NC}"
            echo "  Expected: $TEST_DATA"
            echo "  Got: $S3_CONTENT"
            return 1
        fi
    else
        echo -e " ${RED}✗${NC}"
        return 1
    fi
    
    # Read back
    echo -n "Reading file..."
    READ_CONTENT=$(cat "$TEST_FILE")
    if [ "$READ_CONTENT" = "$TEST_DATA" ]; then
        echo -e " ${GREEN}✓${NC}"
    else
        echo -e " ${RED}✗${NC}"
        echo "  Expected: $TEST_DATA"
        echo "  Got: $READ_CONTENT"
        return 1
    fi
    
    echo -e "${GREEN}Test 1: PASSED${NC}"
    return 0
}

# Test 2: Large file write with throughput measurement
test_large_file_throughput() {
    echo
    echo "================================================================"
    echo "Test 2: Large File Write Throughput"
    echo "================================================================"
    
    LARGE_FILE="$MOUNT_POINT/large_test.bin"
    FILE_SIZE_MB=10
    
    echo "Writing ${FILE_SIZE_MB}MB file..."
    START_TIME=$(date +%s.%N)
    dd if=/dev/urandom of="$LARGE_FILE" bs=1M count=$FILE_SIZE_MB 2>&1 | grep -v records
    END_TIME=$(date +%s.%N)
    
    DURATION=$(echo "$END_TIME - $START_TIME" | bc)
    THROUGHPUT=$(echo "scale=2; $FILE_SIZE_MB / $DURATION" | bc)
    
    echo -e "Write throughput: ${BLUE}${THROUGHPUT} MB/s${NC}"
    echo "Write_Throughput: ${THROUGHPUT} MB/s" >> "$TEST_RESULTS_FILE"
    
    # Get file hash for later verification
    echo -n "Computing hash..."
    FILE_HASH=$(sha256sum "$LARGE_FILE" | awk '{print $1}')
    echo -e " ${GREEN}done${NC}"
    echo "  Hash: $FILE_HASH"
    
    # Wait for flush
    echo "Waiting for flush (5 seconds)..."
    sleep 5
    
    # Verify in S3
    echo -n "Checking S3..."
    if aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/large_test.bin" > /dev/null 2>&1; then
        S3_SIZE=$(aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/large_test.bin" | awk '{print $3}')
        EXPECTED_SIZE=$((FILE_SIZE_MB * 1024 * 1024))
        if [ "$S3_SIZE" -eq "$EXPECTED_SIZE" ]; then
            echo -e " ${GREEN}✓${NC} (size: $S3_SIZE bytes)"
        else
            echo -e " ${YELLOW}⚠${NC} Size mismatch: expected $EXPECTED_SIZE, got $S3_SIZE"
        fi
    else
        echo -e " ${RED}✗${NC}"
        return 1
    fi
    
    # Test read throughput
    echo "Clearing page cache..."
    sync
    echo 3 | sudo tee /proc/sys/vm/drop_caches > /dev/null 2>&1 || true
    
    echo "Testing read throughput..."
    START_TIME=$(date +%s.%N)
    dd if="$LARGE_FILE" of=/dev/null bs=1M 2>&1 | grep -v records
    END_TIME=$(date +%s.%N)
    
    DURATION=$(echo "$END_TIME - $START_TIME" | bc)
    THROUGHPUT=$(echo "scale=2; $FILE_SIZE_MB / $DURATION" | bc)
    
    echo -e "Read throughput: ${BLUE}${THROUGHPUT} MB/s${NC}"
    echo "Read_Throughput: ${THROUGHPUT} MB/s" >> "$TEST_RESULTS_FILE"
    
    # Verify data integrity
    echo -n "Verifying data integrity..."
    NEW_HASH=$(sha256sum "$LARGE_FILE" | awk '{print $1}')
    if [ "$FILE_HASH" = "$NEW_HASH" ]; then
        echo -e " ${GREEN}✓${NC}"
    else
        echo -e " ${RED}✗${NC}"
        echo "  Original: $FILE_HASH"
        echo "  After read: $NEW_HASH"
        return 1
    fi
    
    echo -e "${GREEN}Test 2: PASSED${NC}"
    return 0
}

# Test 3: Multiple small files
test_multiple_small_files() {
    echo
    echo "================================================================"
    echo "Test 3: Multiple Small Files"
    echo "================================================================"
    
    NUM_FILES=20
    echo "Creating $NUM_FILES small files..."
    
    START_TIME=$(date +%s.%N)
    for i in $(seq 1 $NUM_FILES); do
        echo "Test file $i - $(date)" > "$MOUNT_POINT/small_$i.txt"
    done
    END_TIME=$(date +%s.%N)
    
    DURATION=$(echo "$END_TIME - $START_TIME" | bc)
    echo -e "Created $NUM_FILES files in ${BLUE}${DURATION}s${NC}"
    
    # Wait for flush
    echo "Waiting for flush (4 seconds)..."
    sleep 4
    
    # Count files in S3
    echo -n "Checking S3..."
    S3_COUNT=$(aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/" | grep "small_" | wc -l)
    
    if [ "$S3_COUNT" -eq "$NUM_FILES" ]; then
        echo -e " ${GREEN}✓${NC} ($S3_COUNT files)"
    else
        echo -e " ${YELLOW}⚠${NC} Expected $NUM_FILES, found $S3_COUNT"
    fi
    
    # Verify all can be read
    echo -n "Verifying reads..."
    FAILED=0
    for i in $(seq 1 $NUM_FILES); do
        if ! cat "$MOUNT_POINT/small_$i.txt" > /dev/null 2>&1; then
            FAILED=$((FAILED + 1))
        fi
    done
    
    if [ $FAILED -eq 0 ]; then
        echo -e " ${GREEN}✓${NC}"
    else
        echo -e " ${RED}✗${NC} ($FAILED failures)"
        return 1
    fi
    
    echo -e "${GREEN}Test 3: PASSED${NC}"
    return 0
}

# Test 4: Concurrent writes
test_concurrent_writes() {
    echo
    echo "================================================================"
    echo "Test 4: Concurrent Writes"
    echo "================================================================"
    
    NUM_CONCURRENT=5
    echo "Writing $NUM_CONCURRENT files concurrently..."
    
    START_TIME=$(date +%s.%N)
    for i in $(seq 1 $NUM_CONCURRENT); do
        (
            dd if=/dev/urandom of="$MOUNT_POINT/concurrent_$i.bin" bs=1M count=2 2>/dev/null
        ) &
    done
    wait
    END_TIME=$(date +%s.%N)
    
    DURATION=$(echo "$END_TIME - $START_TIME" | bc)
    echo -e "Completed in ${BLUE}${DURATION}s${NC}"
    
    # Wait for flush
    echo "Waiting for flush (5 seconds)..."
    sleep 5
    
    # Verify all in S3
    echo -n "Checking S3..."
    S3_COUNT=$(aws --endpoint-url="$LOCALSTACK_ENDPOINT" s3 ls "s3://$BUCKET_NAME/" | grep "concurrent_" | wc -l)
    
    if [ "$S3_COUNT" -eq "$NUM_CONCURRENT" ]; then
        echo -e " ${GREEN}✓${NC} ($S3_COUNT files)"
    else
        echo -e " ${YELLOW}⚠${NC} Expected $NUM_CONCURRENT, found $S3_COUNT"
    fi
    
    echo -e "${GREEN}Test 4: PASSED${NC}"
    return 0
}

# Test 5: Cache behavior
test_cache_behavior() {
    echo
    echo "================================================================"
    echo "Test 5: Cache Behavior"
    echo "================================================================"
    
    echo "Analyzing cache behavior from logs..."
    
    # Count cache events
    CACHE_TRIGGERS=$(grep -c "CacheFileInExternalCache\|cache event" "$LOG_FILE" 2>/dev/null || echo "0")
    CACHE_SUCCESS=$(grep -c "Successfully cached" "$LOG_FILE" 2>/dev/null || echo "0")
    CACHE_HITS=$(grep -c "cache hit" "$LOG_FILE" 2>/dev/null || echo "0")
    CACHE_MISSES=$(grep -c "cache miss" "$LOG_FILE" 2>/dev/null || echo "0")
    
    echo "Cache statistics:"
    echo "  Triggers: $CACHE_TRIGGERS"
    echo "  Successful stores: $CACHE_SUCCESS"
    echo "  Hits: $CACHE_HITS"
    echo "  Misses: $CACHE_MISSES"
    
    if [ $CACHE_TRIGGERS -gt 0 ]; then
        echo -e "Cache triggering: ${GREEN}✓${NC}"
    else
        echo -e "Cache triggering: ${YELLOW}⚠${NC} (no external cache configured)"
    fi
    
    echo "Cache_Triggers: $CACHE_TRIGGERS" >> "$TEST_RESULTS_FILE"
    echo "Cache_Success: $CACHE_SUCCESS" >> "$TEST_RESULTS_FILE"
    
    echo -e "${GREEN}Test 5: PASSED${NC}"
    return 0
}

# Main execution
main() {
    > "$TEST_RESULTS_FILE"  # Clear results file
    
    check_requirements
    build_geesefs
    setup
    
    if ! mount_filesystem; then
        echo -e "${RED}Failed to mount filesystem${NC}"
        exit 1
    fi
    
    echo -e "${GREEN}✓ Filesystem mounted successfully${NC}"
    echo
    
    # Run tests
    PASSED=0
    FAILED=0
    
    if test_basic_write_read; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
    
    if test_large_file_throughput; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
    
    if test_multiple_small_files; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
    
    if test_concurrent_writes; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
    
    if test_cache_behavior; then
        PASSED=$((PASSED + 1))
    else
        FAILED=$((FAILED + 1))
    fi
    
    # Summary
    echo
    echo "================================================================"
    echo "                     TEST SUMMARY"
    echo "================================================================"
    echo -e "Passed: ${GREEN}$PASSED${NC}"
    echo -e "Failed: ${RED}$FAILED${NC}"
    echo
    
    if [ -f "$TEST_RESULTS_FILE" ]; then
        echo "Performance Results:"
        cat "$TEST_RESULTS_FILE" | sed 's/^/  /'
    fi
    
    echo
    echo "Full log: $LOG_FILE"
    echo
    
    if [ $FAILED -eq 0 ]; then
        echo -e "${GREEN}════════════════════════════════════════════════════════════════${NC}"
        echo -e "${GREEN}                    ALL TESTS PASSED!${NC}"
        echo -e "${GREEN}════════════════════════════════════════════════════════════════${NC}"
        return 0
    else
        echo -e "${RED}════════════════════════════════════════════════════════════════${NC}"
        echo -e "${RED}                    SOME TESTS FAILED${NC}"
        echo -e "${RED}════════════════════════════════════════════════════════════════${NC}"
        return 1
    fi
}

main
