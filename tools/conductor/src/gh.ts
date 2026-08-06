import { execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);

export type OpenPr = {
  number: number;
  title: string;
  url: string;
  headRefName: string;
  headRefOid: string;
  isDraft: boolean;
  reviewDecision: string | null;
  mergeable: string | null;
  mergeStateStatus: string | null;
  statusCheckRollup: Array<StatusCheckRollupItem> | null;
  labels: Array<{ name: string }> | null;
};

/** CheckRun uses `status` + `conclusion`; legacy StatusContext uses `state`. */
type StatusCheckRollupItem = {
  status?: string;
  state?: string;
  conclusion?: string | null;
};

export type PrCommentSummary = {
  number: number;
  title: string;
  url: string;
  headRefName: string;
  /** Head commit SHA — what a review is "at". Reviews name the commit they read. */
  headSha: string;
  isDraft: boolean;
  reviewDecision: string | null;
  mergeable: string | null;
  mergeStateStatus: string | null;
  hasMergeConflict: boolean;
  unresolvedReviewThreads: number;
  issueComments: number;
  checksOk: boolean | null;
  /** Open labels on the PR — carries the conductor in-flight locks. */
  labels: string[];
  /** Commit SHAs that already carry a submitted review (human or agent). */
  reviewedShas: string[];
};

async function ghJson<T>(args: string[]): Promise<T> {
  try {
    const { stdout } = await execFileAsync("gh", args, {
      maxBuffer: 10 * 1024 * 1024,
      env: process.env,
    });
    return JSON.parse(stdout) as T;
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    throw new Error(`gh ${args.join(" ")} failed: ${message}`);
  }
}

async function ghText(args: string[]): Promise<string> {
  const { stdout } = await execFileAsync("gh", args, { env: process.env });
  return stdout.trim();
}

export async function listOpenPrs(): Promise<OpenPr[]> {
  return ghJson<OpenPr[]>([
    "pr",
    "list",
    "--state",
    "open",
    "--json",
    "number,title,url,headRefName,headRefOid,isDraft,reviewDecision,mergeable,mergeStateStatus,statusCheckRollup,labels",
    "--limit",
    "50",
  ]);
}

function rollupItemOk(c: StatusCheckRollupItem): boolean {
  if (c.status !== undefined) {
    if (c.status !== "COMPLETED") return false;
    const conclusion = c.conclusion ?? "";
    return (
      conclusion === "SUCCESS" ||
      conclusion === "NEUTRAL" ||
      conclusion === "SKIPPED"
    );
  }
  if (c.state !== undefined) {
    return c.state === "SUCCESS";
  }
  return false;
}

export function hasMergeConflict(pr: Pick<OpenPr, "mergeable" | "mergeStateStatus">): boolean {
  return pr.mergeable === "CONFLICTING" || pr.mergeStateStatus === "DIRTY";
}

export function rollupOk(pr: OpenPr): boolean | null {
  const checks = pr.statusCheckRollup;
  if (!checks || checks.length === 0) return null;
  return checks.every(rollupItemOk);
}

export async function summarizeOpenPrs(): Promise<PrCommentSummary[]> {
  const prs = await listOpenPrs();
  if (prs.length === 0) return [];

  const owner = await ghText(["repo", "view", "--json", "owner", "--jq", ".owner.login"]);
  const name = await ghText(["repo", "view", "--json", "name", "--jq", ".name"]);

  const summaries: PrCommentSummary[] = [];
  for (const pr of prs) {
    const [prDetail, issueComments] = await Promise.all([
      ghJson<{
        data: {
          repository: {
            pullRequest: {
              reviewThreads: { nodes: Array<{ isResolved: boolean }> };
              reviews: { nodes: Array<{ state: string; commit: { oid: string } | null }> };
            };
          };
        };
      }>([
        "api",
        "graphql",
        "-f",
        `query=query { repository(owner: "${owner}", name: "${name}") { pullRequest(number: ${pr.number}) { reviewThreads(first: 100) { nodes { isResolved } } reviews(last: 50) { nodes { state commit { oid } } } } } }`,
      ]),
      ghJson<{ comments: unknown[] }>([
        "pr",
        "view",
        String(pr.number),
        "--json",
        "comments",
      ]),
    ]);

    const detail = prDetail.data.repository.pullRequest;
    summaries.push({
      number: pr.number,
      title: pr.title,
      url: pr.url,
      headRefName: pr.headRefName,
      headSha: pr.headRefOid,
      isDraft: pr.isDraft,
      reviewDecision: pr.reviewDecision,
      mergeable: pr.mergeable,
      mergeStateStatus: pr.mergeStateStatus,
      hasMergeConflict: hasMergeConflict(pr),
      unresolvedReviewThreads: detail.reviewThreads.nodes.filter((t) => !t.isResolved).length,
      issueComments: issueComments.comments.length,
      checksOk: rollupOk(pr),
      labels: (pr.labels ?? []).map((l) => l.name),
      reviewedShas: submittedReviewShas(detail.reviews.nodes),
    });
  }
  return summaries;
}

/**
 * Commit SHAs carrying a *submitted* review. PENDING reviews are drafts the
 * author has not sent, so they must not count as "this commit was reviewed" —
 * treating them as reviewed would silently drop the PR out of the queue.
 */
export function submittedReviewShas(
  reviews: Array<{ state: string; commit: { oid: string } | null }>,
): string[] {
  const shas = new Set<string>();
  for (const review of reviews) {
    if (review.state === "PENDING") continue;
    if (review.commit?.oid) shas.add(review.commit.oid);
  }
  return [...shas];
}
