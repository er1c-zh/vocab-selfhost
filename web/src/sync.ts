// Sync state machine shared by the app shell. Extracted from the React
// component so the transitions can be unit-tested without a DOM.

export type SyncState = 'idle' | 'syncing' | 'synced' | 'offline' | 'error';

export type SyncStatus = {
  state: SyncState;
  pending: number;
  error: string;
};

export type SyncCallbacks<T> = {
  /** Returns the operations waiting to be uploaded, oldest first. */
  readQueue: () => T[];
  /** Replaces the stored queue. */
  writeQueue: (queue: T[]) => void;
  /** Uploads one operation. Rejects with an Error that has a `name`. */
  runOperation: (op: T) => Promise<void>;
  /** Downloads the server snapshot and applies it locally. */
  pullServerState: () => Promise<void>;
  /** Reports state transitions and dropped operations to the UI. */
  onStatus: (status: SyncStatus) => void;
  /** Reports an operation that had to be dropped (permanent 4xx failure). */
  onDroppedOperation: (op: T, error: Error) => void;
  isOnline: () => boolean;
};

const maxQueuePasses = 3; // guard against a request loop if ops keep arriving
const maxRetryAttempts = 5;
const baseRetryDelayMs = 1500;
const maxRetryDelayMs = 30000;

export type SyncController = {
  sync: () => Promise<void>;
  notifyOnline: () => void;
  notifyOffline: () => void;
  notifyLocalChange: () => void;
  retry: () => void;
  status: () => SyncStatus;
  dispose: () => void;
};

export function createSyncController<T>(callbacks: SyncCallbacks<T>) {
  let syncing = false;
  let resyncRequested = false;
  let retryAttempts = 0;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;
  let disposed = false;
  let current: SyncStatus = { state: 'idle', pending: callbacks.readQueue().length, error: '' };

  const emit = (patch: Partial<SyncStatus>) => {
    if (disposed) return;
    current = { ...current, ...patch, pending: callbacks.readQueue().length };
    callbacks.onStatus(current);
  };

  const isTransient = (error: Error) =>
    error.name === 'NETWORK_ERROR' || error.name === 'SERVER_ERROR' || error.name === 'TIMEOUT_ERROR';

  const scheduleRetry = () => {
    if (retryTimer || disposed) return;
    retryAttempts += 1;
    if (retryAttempts > maxRetryAttempts) return; // stay in error/offline until the user retries or the network returns
    const delay = Math.min(maxRetryDelayMs, baseRetryDelayMs * 2 ** (retryAttempts - 1));
    retryTimer = setTimeout(() => {
      retryTimer = null;
      void sync();
    }, delay);
  };

  const sync = async (): Promise<void> => {
    if (disposed) return;
    if (syncing) {
      resyncRequested = true; // a change arrived mid-sync; run once more after
      return;
    }
    if (!callbacks.isOnline()) {
      emit({ state: 'offline' });
      return;
    }
    syncing = true;
    emit({ state: 'syncing', error: '' });
    try {
      for (let pass = 0; pass < maxQueuePasses; pass += 1) {
        const queue = callbacks.readQueue();
        if (queue.length === 0) break;
        let queueChanged = false;
        for (const op of queue) {
          try {
            await callbacks.runOperation(op);
          } catch (error) {
            const err = error instanceof Error ? error : new Error(String(error));
            if (isTransient(err)) throw err;
            // Permanent failure (bad request, missing card): drop the operation
            // so one broken item cannot block the whole queue forever.
            callbacks.onDroppedOperation(op, err);
          }
          callbacks.writeQueue(callbacks.readQueue().filter((kept) => kept !== op));
          emit({ state: 'syncing' });
          queueChanged = true;
        }
        if (!queueChanged) break;
      }
      await callbacks.pullServerState();
      retryAttempts = 0;
      emit({ state: 'synced', error: '' });
    } catch (error) {
      const err = error instanceof Error ? error : new Error(String(error));
      if (err.name === 'NETWORK_ERROR' || !callbacks.isOnline()) emit({ state: 'offline', error: '' });
      else emit({ state: 'error', error: err.message });
      scheduleRetry();
    } finally {
      syncing = false;
      if (resyncRequested) {
        resyncRequested = false;
        void sync();
      }
    }
  };

  return {
    sync,
    /** Called when the browser reports the network is back. */
    notifyOnline() {
      retryAttempts = 0;
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      emit({ state: current.state === 'syncing' ? 'syncing' : 'idle' });
      void sync();
    },
    /** Called when the browser reports the network is gone. */
    notifyOffline() {
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      emit({ state: 'offline' });
    },
    /** Called after enqueuing a local change. */
    notifyLocalChange() {
      if (!retryTimer) void sync();
    },
    retry() {
      retryAttempts = 0;
      if (retryTimer) {
        clearTimeout(retryTimer);
        retryTimer = null;
      }
      void sync();
    },
    status: () => current,
    dispose() {
      disposed = true;
      if (retryTimer) clearTimeout(retryTimer);
    },
  } satisfies SyncController;
}
