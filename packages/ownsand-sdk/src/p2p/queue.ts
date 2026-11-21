// SPDX-License-Identifier: Apache-2.0

/**
 * A single-consumer async queue with an end or a failure. It does not limit
 * its size itself; it reports how many items and bytes are queued so that the
 * producer, which cannot wait (a data channel receive callback), can fail
 * the stream instead of growing the queue without bound.
 */
export class AsyncQueue<T> implements AsyncIterable<T> {
  readonly #items: { item: T; size: number }[] = [];
  #bytes = 0;
  #waiter: (() => void) | undefined;
  #done = false;
  #error: unknown;
  #failed = false;

  /** The number of queued items that the consumer has not taken yet. */
  get length(): number {
    return this.#items.length;
  }

  /** The sum of the sizes of the queued items. */
  get bytes(): number {
    return this.#bytes;
  }

  /** Queues an item; `size` is its weight in bytes for `bytes`. */
  push(item: T, size = 0): void {
    if (this.#done || this.#failed) {
      return;
    }
    this.#items.push({ item, size });
    this.#bytes += size;
    this.#wake();
  }

  end(): void {
    this.#done = true;
    this.#wake();
  }

  fail(error: unknown): void {
    if (this.#done || this.#failed) {
      return;
    }
    this.#failed = true;
    this.#error = error;
    this.#wake();
  }

  #wake(): void {
    const waiter = this.#waiter;
    this.#waiter = undefined;
    waiter?.();
  }

  async *[Symbol.asyncIterator](): AsyncIterator<T> {
    for (;;) {
      const next = this.#items.shift();
      if (next !== undefined) {
        this.#bytes -= next.size;
        yield next.item;
        continue;
      }
      if (this.#failed) {
        throw this.#error;
      }
      if (this.#done) {
        return;
      }
      await new Promise<void>((resolve) => {
        this.#waiter = resolve;
      });
    }
  }
}
