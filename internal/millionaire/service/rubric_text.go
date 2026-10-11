package service

// RubricV1 is the AUTHORITATIVE strength rubric (SPEC §2, question engine
// v2): cmd/label -export writes it verbatim at the head of every batch
// file and to label/rubric.md, and the README quotes it verbatim — the
// labeling AGENTS consume exactly this text. Do not inline edits into
// batches; change it here (one source).
//
// Band anchors (SPEC §2): 1–3 = pub-quiz round 1 (most adults know),
// 4–6 = solid general knowledge, 7–9 = keen-reader knowledge,
// 10–12 = specialist / niche, 13–15 = expert / obscure.
const RubricV1 = `# Millionaire question-strength rubric (v1)

You are rating quiz questions for a "Who Wants to Be a Millionaire" ladder of
15 levels. Each question gets ONE integer strength from 1 to 15 — how hard it
is for a typical ADULT to recall the answer cold, in no more than a few
seconds, on the first attempt, with no external help. Category/subject never
matters on its own: some history is easy, some science is expert-grade.

## Calibration anchors (memorize these nine)

| Band | Meaning | Anchored examples |
|---|---|---|
| 1–3 | Pub-quiz round 1 — most adults know this | "Which planet is known as the Red Planet?" (2); "How many minutes are in an hour?" (1); "Rio de Janeiro is a city in which country?" (3) |
| 4–6 | Solid general knowledge | "Who wrote 'Romeo and Juliet'?" (4); "What is the capital of Australia?" (5); "Which element has the chemical symbol Fe?" (6) |
| 7–9 | Keen-reader knowledge — the proverbial bar-fly quiz winner | "Who painted 'The Garden of Earthly Delights'?" (8); "In which year did the Chernobyl disaster occur?" (7); "Which mountain is the highest in South America?" (9) |
| 10–12 | Specialist / niche | "Under which treaty was the Itaipu Dam jointly constructed?" (11); "Who won the 1978 Nobel in economics work on institutions?" (10); "What is the SI unit of magnetic flux density?" (12) |
| 13–15 | Expert / obscure — deep academic or trivia-savant knowledge | "The chemical structure of which alkaloid did Robert Robinson prove in 1947?" (14); "Which 4th-century Sassanid ruler issued a gold-coins reform known as the 'Khosrow reform'?" (13); "Name the secondary host of the trematode Echinostoma revolutum" (15) |

## Rating procedure (do it explicitly, question by question)

1. Ask: could ~70% of typical adults answer this in <3s? → it is 1–3.
2. If not, could the average educated NEWS-reader? → 4–6; a "reads books and
   remembers detail" person → 7–9.
3. Beyond keen-reader: does it need an active hobby, a degree, or fandom?
   → 10–12; graduate-level, one-page-of-Wikipedia-deep or trivia-savant
   recall → 13–15.
4. Prefer the ANCHOR you are most confident of, not the average between two.
5. Use the FULL range: a batch of 20 questions should normally spread over
   several bands, but never INFLATE or DEFLATE to force spread. If a batch is
   all easy, so be it.

## What does NOT change the rating

- Long-winded or multi-line phrasing (rate the FACT, not the wording).
- Wrong or odd options in the list — you rate the question, not the answer set.
- Which source the question came from or which file/lines it is in.
- Whether YOU personally knew it. A widely-known fact you forgot is still low.
- Obscure celebrity minutiae: famous NAME = easy; the 4th studio album of a
  famous band = mid-to-hard depending on how deep the fanbase goes.

## Output

JSON Lines, one object per question, exactly:

  {"question_id": "<the id given under each question>", "rating": <1-15>}

Rate EVERY question in the batch exactly once. Integers only, 1..15.
The orchestrator imports your file back with cmd/label -import and validates.
`
