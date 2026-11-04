import assert from 'node:assert/strict';
import test from 'node:test';
import {splitAccountCredits} from './credit-availability.ts';

const credits = {
  online: 100,
  cooling: 200,
  disabled: 300,
  manual: 400,
  unknown: 500,
  missing: undefined,
};

const accounts = Object.keys(credits).map((uid) => ({
  uid,
  credits: 0,
  cooling: uid === 'cooling',
  disabled: uid === 'disabled',
  manual_disabled: uid === 'manual',
  pool_unknown: uid === 'unknown',
}));

test('cooling credits remain available; blocked and unknown credits do not', () => {
  const got = splitAccountCredits(accounts, (account) => credits[account.uid]);
  assert.equal(got.available, 300);
  assert.deepEqual(got.availableRows.map((a) => a.uid).sort(), ['cooling', 'online']);
  assert.equal(got.unavailable, 1200);
  assert.deepEqual(got.unavailableRows.map((a) => a.uid).sort(), ['disabled', 'manual', 'unknown']);
});

test('missing balance is excluded instead of counted as zero', () => {
  const got = splitAccountCredits(accounts, (account) => credits[account.uid]);
  assert.ok(!got.availableRows.some((a) => a.uid === 'missing'));
  assert.ok(!got.unavailableRows.some((a) => a.uid === 'missing'));
});
