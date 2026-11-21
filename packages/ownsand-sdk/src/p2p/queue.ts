// SPDX-License-Identifier: Apache-2.0

/** An unbounded single-consumer async queue with an end or a failure. */
export class AsyncQueue<T> implements AsyncIterable<T> {
  readonly #items: T[] = [];
  #waiter: (() => void) | undefined;
  #done = false;
  #error: unknown;
  #failed = false;

  push(item: T): void {
    if (this.#done || this.#failed) {
      return;
    }
    this.#items.push(item);
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
      if (this.#items.length > 0) {
        yield this.#items.shift()!;
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
