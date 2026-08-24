// Regression coverage for task #698 item 2 — the skill send-now path must
// carry pending attachments on the send_message frame and clear the badge;
// sibling of the #679/#691 attachment coverage in attachment-send.spec.ts,
// which fixed every other send path (send/sendFast/interrupt/inject) but
// missed this one.

import { test, expect, Page } from "@playwright/test";
import { setupMockWS, sendMockWSMessage, waitForWSSend } from "./helpers/mock-ws";
import { makeSession } from "./helpers/fixtures";

test.beforeEach(async ({ page }) => {
  await setupMockWS(page);
  await page.route("/auth/check", (route) =>
    route.fulfill({ status: 200, body: "OK" })
  );
});

async function openSession(page: Page, id: string, title: string) {
  await page.goto("/");
  await sendMockWSMessage(page, {
    type: "sessions_list",
    payload: [makeSession({ ID: id, Title: title })],
  });
  await expect(page.getByText(title).first()).toBeVisible({ timeout: 5000 });
  await page.getByText(title).first().click();
  await expect(page.getByTestId("chat-input-textarea")).toBeEnabled({ timeout: 5000 });
}

// Drop a file onto the composer and wait for its badge to appear.
async function dropFile(page: Page, name: string, contents: string) {
  await page.evaluate(({ name, contents }) => {
    const dropArea = document.querySelector("div[class*='rounded-2xl'][class*='bg-base-overlay']");
    if (!dropArea) throw new Error("composer drop area not found");
    const file = new File([contents], name, { type: "image/png" });
    const dt = new DataTransfer();
    dt.items.add(file);
    dropArea.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true }));
  }, { name, contents });
  await expect(page.getByText(name).first()).toBeVisible({ timeout: 5000 });
}

function b64(s: string): string {
  return Buffer.from(s, "utf8").toString("base64");
}

function payloadOf(msg: { payload?: unknown }): Record<string, unknown> {
  return (msg.payload ?? {}) as Record<string, unknown>;
}

function attachmentsOf(msg: { payload?: unknown }): { fileName: string; mimeType: string; data: string }[] {
  return ((payloadOf(msg).attachments ?? []) as { fileName: string; mimeType: string; data: string }[]);
}

test("skill send-now carries pending attachments and clears the badge", async ({ page }) => {
  await openSession(page, "skill-att-1", "Skill Attachment Test");

  // Send a skill with instructions — this will be the send_message content.
  await sendMockWSMessage(page, {
    type: "skills",
    payload: {
      skills: [
        { name: "review", description: "Run the full review checklist", path: "/review", instructions: "Run the full review checklist" },
      ],
    },
  });

  const contents = "screenshot data";
  await dropFile(page, "notes.png", contents);

  // Type "/review" to open the slash menu.
  await page.getByTestId("chat-input-textarea").pressSequentially("/review");

  // Wait for the menu item to appear.
  await expect(page.getByText("Run the full review checklist")).toBeVisible({ timeout: 5000 });

  // Press Enter to invoke the skill with sendNow=true.
  await page.getByTestId("chat-input-textarea").press("Enter");

  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("Run the full review checklist");
  expect(attachmentsOf(msg)).toEqual([
    { fileName: "notes.png", mimeType: "image/png", data: b64(contents) },
  ]);

  // Badge clears because the attachment actually went out on the frame.
  await expect(page.getByText("notes.png")).toBeHidden();
});

test("skill without instructions falls back to the /name command and still carries the attachment", async ({ page }) => {
  await openSession(page, "skill-att-2", "Skill Attachment No Instr");

  // Send a skill WITHOUT instructions — the content must be the command string.
  await sendMockWSMessage(page, {
    type: "skills",
    payload: {
      skills: [
        { name: "cleanup", description: "Clean up temp files", path: "/cleanup" },
      ],
    },
  });

  const contents = "cleanup data";
  await dropFile(page, "cleanup.png", contents);

  await page.getByTestId("chat-input-textarea").pressSequentially("/cleanup");
  await expect(page.getByText("Clean up temp files")).toBeVisible({ timeout: 5000 });
  await page.getByTestId("chat-input-textarea").press("Enter");

  const msg = await waitForWSSend(page, "send_message");
  const payload = payloadOf(msg);
  expect(payload.content).toBe("/cleanup");
  expect(attachmentsOf(msg)).toEqual([
    { fileName: "cleanup.png", mimeType: "image/png", data: b64(contents) },
  ]);

  await expect(page.getByText("cleanup.png")).toBeHidden();
});
