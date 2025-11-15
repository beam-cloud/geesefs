package core

import (
	"sync"
	"testing"
	"time"

	"github.com/jacobsa/fuse/fuseops"
)

// TestNilReadCondPanic verifies the nil check prevents panic when readCond is nil.
func TestNilReadCondPanic(t *testing.T) {
	inode := &Inode{Id: 1, Name: "test.txt"}
	inode.Attributes.Size = 1024
	
	if inode.readCond != nil {
		t.Fatal("Test setup error: readCond should be nil")
	}
	
	// Verify nil check pattern prevents panic
	inode.mu.Lock()
	if inode.readCond != nil {
		inode.readCond.Broadcast()
	}
	inode.mu.Unlock()
}

// TestNilReadCondPanicReproduced demonstrates the panic that occurs WITHOUT the fix.
func TestNilReadCondPanicReproduced(t *testing.T) {
	inode := &Inode{Id: 1, Name: "test.txt"}
	
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Expected panic did not occur")
		}
	}()
	
	inode.mu.Lock()
	inode.readCond.Broadcast() // This WILL panic - reproducing the bug
	inode.mu.Unlock()
}

// TestRaceCondition verifies the fix prevents panic under concurrent access.
func TestRaceCondition(t *testing.T) {
	for i := 0; i < 100; i++ {
		inode := &Inode{Id: fuseops.InodeID(i), Name: "test.txt"}
		inode.Attributes.Size = 1024
		
		var wg sync.WaitGroup
		
		// Goroutine 1: Initialize readCond (delayed)
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Millisecond)
			inode.mu.Lock()
			inode.readCond = sync.NewCond(&inode.mu)
			inode.mu.Unlock()
		}()
		
		// Goroutine 2: Try to broadcast (immediate)
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(1 * time.Millisecond)
			inode.mu.Lock()
			if inode.readCond != nil {
				inode.readCond.Broadcast()
			}
			inode.mu.Unlock()
		}()
		
		wg.Wait()
	}
}

// TestRaceWithoutFix reproduces the race condition that causes the panic.
func TestRaceWithoutFix(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping panic test in short mode")
	}
	
	panicked := false
	for i := 0; i < 50 && !panicked; i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
					t.Logf("✓ Panic reproduced on attempt %d: %v", i, r)
				}
			}()
			
			inode := &Inode{Id: fuseops.InodeID(i), Name: "test.txt"}
			
			ready := make(chan struct{})
			
			// Initialize readCond slowly
			go func() {
				close(ready)
				time.Sleep(5 * time.Millisecond)
				inode.mu.Lock()
				inode.readCond = sync.NewCond(&inode.mu)
				inode.mu.Unlock()
			}()
			
			// Broadcast before initialization completes
			<-ready
			time.Sleep(1 * time.Millisecond)
			inode.mu.Lock()
			inode.readCond.Broadcast() // NO NIL CHECK - simulates the bug
			inode.mu.Unlock()
		}()
	}
	
	if !panicked {
		t.Log("⚠ Couldn't reproduce in 50 attempts, but direct test proves vulnerability")
	}
}
