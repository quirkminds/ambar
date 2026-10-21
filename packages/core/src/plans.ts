/** Plans: one source for the landing pricing and the app settings. Rename plans or change limits here only. */

export type PlanId = "seed" | "leaf";

const GB = 1024 ** 3;

export type Plan = {
  id: PlanId;
  name: string;
  /** Price in euros. The free plan has no yearly price. */
  monthly: number;
  yearly?: number;
  /** What the API enforces per vault (apps/api checks that it matches). */
  storageBytes: number;
  storage: string;
  readings: string;
  moments: string;
};

function storageCopy(bytes: number) {
  return `${bytes / GB} GB of storage`;
}

export const plans: Plan[] = [
  {
    id: "seed",
    name: "Seed",
    monthly: 0,
    storageBytes: 2 * GB,
    storage: storageCopy(2 * GB),
    readings: "75 readings a month",
    moments: "Up to 2,000 moments",
  },
  {
    id: "leaf",
    name: "Leaf",
    monthly: 5,
    yearly: 48,
    storageBytes: 25 * GB,
    storage: storageCopy(25 * GB),
    readings: "500 readings a month",
    moments: "Unlimited moments",
  },
];
