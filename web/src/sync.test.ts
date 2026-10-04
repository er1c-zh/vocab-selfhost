import { describe, expect, it, vi } from 'vitest';
import { createSyncController, type SyncStatus } from './sync';

type Op = { id: string };

function failure(name: string, message = name): Error {
  const error = new Error(message);
  error.name = name;
  return error;
}

type RunOp = (op: Op) => Promise<void>;

function setup(overrides: Partial<Parameters<typeof createSyncController<Op>>[0]> = {}) {
  let queue: Op[] = [];
  const statuses: SyncStatus[] = [];
  const dropped: { op: Op; error: Error }[] = [];
  const runOperation = ((overrides.runOperation ?? vi.fn().mockResolvedValue(undefined)) as RunOp) as ReturnType<typeof vi.fn>;
  const controller = createSyncController<Op>({
    readQueue: () => queue,
    writeQueue: (next) => {
      queue = next;
    },
    runOperation: runOperation as RunOp,
    pullServerState: vi.fn().mockResolvedValue(undefined),
    onStatus: (status) => statuses.push({ ...status }),
    onDroppedOperation: (op, error) => dropped.push({ op, error }),
    isOnline: () => true,
    ...overrides,
  });
  return { controller, statuses, dropped, setQueue: (ops: Op[]) => (queue = ops), runOperation };
}

describe('sync controller', () => {
  it('syncs the queue, pulls the server state and ends in synced', async () => {
    const { controller, statuses, setQueue } = setup();
    setQueue([{ id: 'a' }, { id: 'b' }]);
    await controller.sync();
    expect(controller.status()).toMatchObject({ state: 'synced', pending: 0, error: '' });
    expect(statuses[statuses.length - 1]).toMatchObject({ state: 'synced' });
    expect(statuses.some((s) => s.state === 'syncing')).toBe(true);
  });

  it('stays idle when there is nothing to sync but still reports synced after pull', async () => {
    const { controller } = setup();
    await controller.sync();
    expect(controller.status().state).toBe('synced');
  });

  it('does not run while offline and reports offline', async () => {
    const { controller, statuses, runOperation, setQueue } = setup({ isOnline: () => false });
    setQueue([{ id: 'a' }]);
    await controller.sync();
    expect(controller.status().state).toBe('offline');
    expect(runOperation).not.toHaveBeenCalled();
    expect(statuses.some((s) => s.state === 'syncing')).toBe(false);
  });

  it('transitions to offline on a network error and schedules a capped retry', async () => {
    vi.useFakeTimers();
    try {
      const runOperation = vi.fn().mockRejectedValue(failure('NETWORK_ERROR'));
      const { controller, statuses, setQueue } = setup({ runOperation });
      setQueue([{ id: 'a' }]);
      await controller.sync();
      expect(controller.status().state).toBe('offline');
      expect(statuses.some((s) => s.state === 'syncing')).toBe(true);
      // Retry attempts back off and eventually stop.
      for (let i = 0; i < 6; i += 1) await vi.advanceTimersByTimeAsync(31000);
      expect(runOperation.mock.calls.length).toBeLessThanOrEqual(6);
      expect(controller.status().state).toBe('offline');
    } finally {
      vi.useRealTimers();
    }
  });

  it('transitions to error on a server error and keeps the queue', async () => {
    const runOperation = vi.fn().mockRejectedValue(failure('SERVER_ERROR', 'boom'));
    const { controller, statuses, setQueue } = setup({ runOperation });
    setQueue([{ id: 'a' }]);
    await controller.sync();
    expect(controller.status().state).toBe('error');
    expect(controller.status().error).toBe('boom');
    expect(controller.status().pending).toBe(1);
    expect(statuses[statuses.length - 1].error).toBe('boom');
  });

  it('drops permanently failing operations but keeps syncing the rest', async () => {
    const runOperation = vi.fn(async (op: Op) => {
      if (op.id === 'bad') throw failure('API_ERROR', 'word must contain 1-100 characters');
    });
    const { controller, dropped, setQueue } = setup({ runOperation });
    setQueue([{ id: 'bad' }, { id: 'good' }]);
    await controller.sync();
    expect(controller.status().state).toBe('synced');
    expect(controller.status().pending).toBe(0);
    expect(dropped).toHaveLength(1);
    expect(dropped[0].op.id).toBe('bad');
    expect(runOperation).toHaveBeenCalledTimes(2);
  });

  it('re-syncs once when a change arrives mid-sync instead of running concurrently', async () => {
    const gate = { release: () => {} };
    const firstGate = new Promise<void>((resolve) => { gate.release = resolve; });
    const calls: string[] = [];
    const runOperation = vi.fn(async (op: Op) => {
      calls.push(op.id);
      if (calls.length === 1) await firstGate;
    });
    const { controller, setQueue } = setup({ runOperation });
    setQueue([{ id: 'first' }]);
    const firstSync = controller.sync();
    const secondSync = controller.sync(); // while the first is still running
    setQueue([{ id: 'second' }]); // arrives mid-sync
    gate.release();
    await Promise.all([firstSync, secondSync]);
    expect(calls).toEqual(['first', 'second']);
    expect(controller.status().state).toBe('synced');
    expect(controller.status().pending).toBe(0);
  });

  it('notifyOnline resets backoff and retries immediately', async () => {
    vi.useFakeTimers();
    try {
      let online = true;
      let shouldFail = true;
      const runOperation = vi.fn(async () => {
        if (shouldFail) throw failure('NETWORK_ERROR');
      });
      const { controller, setQueue } = setup({ runOperation, isOnline: () => online });
      setQueue([{ id: 'a' }]);
      await controller.sync();
      expect(controller.status().state).toBe('offline');
      shouldFail = false;
      controller.notifyOnline();
      await vi.advanceTimersByTimeAsync(0);
      expect(controller.status().state).toBe('synced');
      expect(online).toBe(true);
    } finally {
      vi.useRealTimers();
    }
  });

  it('notifyOffline stops the retry loop and marks the state offline', async () => {
    vi.useFakeTimers();
    try {
      const runOperation = vi.fn(async () => {
        throw failure('NETWORK_ERROR');
      });
      const { controller, setQueue } = setup({ runOperation });
      setQueue([{ id: 'a' }]);
      await controller.sync();
      expect(controller.status().state).toBe('offline');
      controller.notifyOffline();
      await vi.advanceTimersByTimeAsync(60000);
      expect(controller.status().state).toBe('offline');
    } finally {
      vi.useRealTimers();
    }
  });

  it('a transient failure does not lose already-uploaded operations', async () => {
    const queue = [{ id: 'a' }, { id: 'b' }, { id: 'c' }];
    let stored: Op[] = [...queue];
    const readQueue = () => stored;
    const writeQueue = (next: Op[]) => (stored = next);
    const runOperation = vi.fn(async (op: Op) => {
      if (op.id === 'c') throw failure('SERVER_ERROR');
    });
    const { controller } = setup({ readQueue, writeQueue, runOperation });
    await controller.sync();
    expect(stored.map((op) => op.id)).toEqual(['c']);
    expect(controller.status().state).toBe('error');
  });
});
